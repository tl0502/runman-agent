package netmigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/network"
	_ "github.com/glebarez/go-sqlite"
	nettypes "go.podman.io/common/libnetwork/types"
)

const (
	networkDir   = "/etc/containers/networks"
	podmanSocket = "unix:///run/podman/podman.sock"
)

type podmanBackend struct{ dir string }

func (b *podmanBackend) inspect(ctx context.Context, name string) (State, error) {
	return Inspect(ctx, name)
}

func parseState(raw json.RawMessage) (State, error) {
	var doc struct {
		Interface string                             `json:"network_interface"`
		Subnets   []struct{ Subnet, Gateway string } `json:"subnets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return State{}, fmt.Errorf("parse network: %w", err)
	}
	st := State{Exists: true, Interface: doc.Interface, Config: raw}
	v4, v6 := 0, 0
	for _, s := range doc.Subnets {
		if strings.Contains(s.Subnet, ":") {
			st.IPv6, st.Gateway6 = s.Subnet, s.Gateway
			v6++
		} else {
			st.IPv4, st.Gateway4 = s.Subnet, s.Gateway
			v4++
		}
	}
	if v4 > 1 || v6 > 1 {
		return State{}, fmt.Errorf("不支持迁移含多个同族子网的网络，未修改网络")
	}
	return st, nil
}

func (b *podmanBackend) snapshot(ctx context.Context, name string, cur State) (*snapshot, error) {
	s := &snapshot{Network: name, State: cur}
	if !cur.Exists {
		return s, nil
	}
	// Inspect omits unknown fields. The actual file is the authoritative snapshot.
	raw, err := os.ReadFile(filepath.Join(b.dir, name+".json"))
	if err != nil {
		return nil, fmt.Errorf("读取完整网络配置失败，未修改网络：%w", err)
	}
	st, err := parseState(raw)
	if err != nil {
		return nil, err
	}
	var header struct{ Name, Driver string }
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, err
	}
	if header.Name != name || header.Driver != "bridge" || !sameShape(st, stateOptions(name, cur)) {
		return nil, fmt.Errorf("网络配置发生变化或不是受支持的 bridge 网络，未修改网络")
	}
	s.State = st
	out, err := run(ctx, "podman", "ps", "-a", "--no-trunc", "--filter", "network="+name, "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("列出容器失败：%w（%s）", err, strings.TrimSpace(out))
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return s, nil
	}
	// CLI connect cannot accept the custom bandwidth options. Verify the API before
	// any disconnect, then use its full PerNetworkOptions payload when reconnecting.
	apiCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	apiCtx, err = bindings.NewConnection(apiCtx, podmanSocket)
	if err == nil {
		if client, e := bindings.GetClient(apiCtx); e == nil {
			client.Client.CloseIdleConnections()
		}
	}
	cancel()
	if err != nil {
		return nil, err
	}
	stateDB, err := openStateDB(ctx, ids[0])
	if err != nil {
		return nil, err
	}
	defer stateDB.Close()
	for _, id := range ids {
		var name, cfgJSON string
		if err := stateDB.QueryRowContext(ctx, "SELECT Name, JSON FROM ContainerConfig WHERE ID=?", id).Scan(&name, &cfgJSON); err != nil {
			return nil, fmt.Errorf("读取容器 %s 的持久网络配置失败，未修改网络：%w", id, err)
		}
		var cfg struct {
			Networks map[string]nettypes.PerNetworkOptions `json:"newNetworks"`
		}
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
			return nil, err
		}
		opts, ok := cfg.Networks[s.Network]
		if !ok {
			return nil, fmt.Errorf("容器 %s 缺少持久网络配置，未修改网络", name)
		}
		out, err := run(ctx, "podman", "inspect", id, "--format", "{{json .NetworkSettings.Networks}}")
		if err != nil {
			return nil, fmt.Errorf("读取容器 %s 的地址失败：%w", name, err)
		}
		var nets map[string]struct{ IPAddress, GlobalIPv6Address, MacAddress string }
		if err := json.Unmarshal([]byte(out), &nets); err != nil {
			return nil, err
		}
		runtime, ok := nets[s.Network]
		if !ok {
			return nil, fmt.Errorf("容器 %s 的网络绑定发生变化，未修改网络", name)
		}
		c := Container{Name: name, ID: id, IPv4: runtime.IPAddress, IPv6: runtime.GlobalIPv6Address, MAC: runtime.MacAddress, Options: opts}
		// Running containers may have dynamically allocated addresses. Pin them for
		// migration. Stopped containers use the persistent static IP/MAC instead.
		has4, has6 := false, false
		for _, ip := range opts.StaticIPs {
			if ip.To4() != nil {
				c.IPv4, has4 = ip.String(), true
			} else {
				c.IPv6, has6 = ip.String(), true
			}
		}
		if !has4 && net.ParseIP(c.IPv4) != nil {
			c.Options.StaticIPs = append(c.Options.StaticIPs, net.ParseIP(c.IPv4))
		}
		if !has6 && net.ParseIP(c.IPv6) != nil {
			c.Options.StaticIPs = append(c.Options.StaticIPs, net.ParseIP(c.IPv6))
		}
		if len(opts.StaticMAC) > 0 {
			c.MAC = net.HardwareAddr(opts.StaticMAC).String()
		} else if c.MAC != "" {
			mac, err := net.ParseMAC(c.MAC)
			if err != nil {
				return nil, err
			}
			c.Options.StaticMAC = nettypes.HardwareAddr(mac)
		}
		s.Containers = append(s.Containers, c)
	}
	return s, nil
}

// Podman inspect intentionally omits driver options and persistent static IPs.
// Read only those network fields from the SQLite state. Unsupported backends or
// schemas fail before mutation; the Podman database is never modified here.
func openStateDB(ctx context.Context, containerID string) (*sql.DB, error) {
	out, err := run(ctx, "podman", "info", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var info struct {
		Host  struct{ DatabaseBackend string }
		Store struct {
			GraphRoot, RunRoot string
			TransientStore     bool
		}
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return nil, err
	}
	if info.Host.DatabaseBackend != "sqlite" {
		return nil, fmt.Errorf("网络迁移需要可完整读取的 Podman SQLite 配置（当前 %s），未修改网络", info.Host.DatabaseBackend)
	}
	base := info.Store.GraphRoot
	if info.Store.TransientStore {
		base = info.Store.RunRoot
	}
	var errs []error
	for _, path := range []string{filepath.Join(base, "db.sql"), filepath.Join(base, "libpod", "db.sql")} {
		if _, err := os.Stat(path); err != nil {
			errs = append(errs, err)
			continue
		}
		dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
		d, err := sql.Open("sqlite", dsn)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		d.SetMaxOpenConns(1)
		var found int
		err = d.QueryRowContext(ctx, "SELECT 1 FROM ContainerConfig WHERE ID=?", containerID).Scan(&found)
		if err == nil {
			return d, nil
		}
		d.Close()
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("无法完整读取 Podman 持久网络配置，未修改网络：%w", errors.Join(errs...))
}

func targetConfig(raw json.RawMessage, want Options) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	delete(doc, "id")
	delete(doc, "created")
	delete(doc, "containers")
	var subnets []map[string]json.RawMessage
	if err := json.Unmarshal(doc["subnets"], &subnets); err != nil {
		return nil, err
	}
	var next []map[string]json.RawMessage
	for _, f := range []struct{ cidr, gateway string }{{want.IPv4, want.Gateway4}, {want.IPv6, want.Gateway6}} {
		if f.cidr == "" {
			continue
		}
		subnet := map[string]json.RawMessage{}
		for _, old := range subnets {
			var cidr string
			if json.Unmarshal(old["subnet"], &cidr) == nil && cidr == f.cidr {
				subnet = old
				break
			}
		}
		subnet["subnet"], _ = json.Marshal(f.cidr)
		if f.gateway != "" {
			subnet["gateway"], _ = json.Marshal(f.gateway)
		}
		next = append(next, subnet)
	}
	doc["subnets"], _ = json.Marshal(next)
	doc["ipv6_enabled"], _ = json.Marshal(want.IPv6 != "")
	if want.Interface != "" {
		doc["network_interface"], _ = json.Marshal(want.Interface)
	}
	if want.IPv6 == "" && len(doc["options"]) > 0 {
		var opts map[string]json.RawMessage
		if err := json.Unmarshal(doc["options"], &opts); err != nil {
			return nil, err
		}
		delete(opts, "outbound_addr6")
		doc["options"], _ = json.Marshal(opts)
	}
	return json.Marshal(doc)
}

func (b *podmanBackend) create(ctx context.Context, want Options, raw json.RawMessage) error {
	out, err := run(ctx, "podman", append([]string{"network", "create"}, createArgs(want)...)...)
	if err != nil {
		return fmt.Errorf("%w（%s）", err, strings.TrimSpace(out))
	}
	if len(raw) == 0 {
		return nil
	}
	path := filepath.Join(b.dir, want.Network+".json")
	created, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc, defaults map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if err := json.Unmarshal(created, &defaults); err != nil {
		return err
	}
	// Target switches get a new identity; rollback keeps the exact old identity.
	for _, key := range []string{"name", "id", "created", "network_interface"} {
		if _, ok := doc[key]; !ok {
			if v, exists := defaults[key]; exists {
				doc[key] = v
			}
		}
	}
	delete(doc, "containers")
	data, err := json.MarshalIndent(doc, "", "     ")
	if err != nil {
		return err
	}
	// Atomic rename also invalidates the long-lived Podman service's directory cache.
	return atomicWrite(path, append(data, '\n'), 0o644)
}

func (b *podmanBackend) membership(ctx context.Context, network, name string) (bool, bool, error) {
	out, err := run(ctx, "podman", "inspect", name, "--format", `{"running":{{.State.Running}},"networks":{{json .NetworkSettings.Networks}}}`)
	if err != nil {
		return false, false, err
	}
	var doc struct {
		Running  bool
		Networks map[string]struct{ IPAddress string }
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return false, false, err
	}
	n, present := doc.Networks[network]
	return present, present && doc.Running && n.IPAddress == "", nil
}

func (b *podmanBackend) disconnect(ctx context.Context, network, name string) error {
	present, _, err := b.membership(ctx, network, name)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	out, err := run(ctx, "podman", "network", "disconnect", "--force", network, name)
	if err != nil {
		return fmt.Errorf("%w（%s）", err, strings.TrimSpace(out))
	}
	return nil
}

func (b *podmanBackend) remove(ctx context.Context, network string) error {
	out, err := run(ctx, "podman", "network", "rm", network)
	if err != nil {
		return fmt.Errorf("%w（%s）", err, strings.TrimSpace(out))
	}
	return nil
}

func (b *podmanBackend) connect(ctx context.Context, netName string, c Container, withV6 bool) error {
	present, stale, err := b.membership(ctx, netName, c.Name)
	if err != nil {
		return err
	}
	if present && !stale {
		return nil
	}
	if stale {
		if err := b.disconnect(ctx, netName, c.Name); err != nil {
			return err
		}
	}
	opts := c.Options
	// Podman appends its short ID during every API connect. Send each user alias
	// once and let Podman add that automatic alias, avoiding growth on each switch.
	opts.Aliases = nil
	seen := map[string]bool{}
	for _, alias := range c.Options.Aliases {
		if len(c.ID) >= 12 && alias == c.ID[:12] {
			continue
		}
		if !seen[alias] {
			opts.Aliases = append(opts.Aliases, alias)
			seen[alias] = true
		}
	}
	opts.StaticIPs = nil
	for _, ip := range c.Options.StaticIPs {
		if ip.To4() != nil || withV6 {
			opts.StaticIPs = append(opts.StaticIPs, ip)
		}
	}
	apiCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	apiCtx, err = bindings.NewConnection(apiCtx, podmanSocket)
	if err != nil {
		return err
	}
	if client, e := bindings.GetClient(apiCtx); e == nil {
		defer client.Client.CloseIdleConnections()
	}
	return network.Connect(apiCtx, netName, c.Name, &opts)
}

func (b *podmanBackend) journal(name string) string {
	return filepath.Join(b.dir, name+".json.migration")
}

func (b *podmanBackend) load(name string) (*snapshot, error) {
	data, err := os.ReadFile(b.journal(name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse recovery journal: %w", err)
	}
	return &s, nil
}

func (b *podmanBackend) save(s *snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(b.journal(s.Network), append(data, '\n'), 0o600)
}

func (b *podmanBackend) clear(name string) error {
	err := os.Remove(b.journal(name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".netmigrate-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
