// Package netmigrate switches a Podman bridge between IPv4 and dual stack.
// Container processes remain running; connections are interrupted during the switch.
package netmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	nettypes "go.podman.io/common/libnetwork/types"
)

const operationTimeout = 5 * time.Minute

// Options describes the target network. Changing an existing IP subnet is unsupported.
type Options struct {
	Network   string
	Interface string
	IPv4      string
	Gateway4  string
	IPv6      string // Empty removes IPv6.
	Gateway6  string
}

type Result struct {
	Containers int
	Downtime   time.Duration
	Rebuilt    bool
}

// Container includes persistent configuration as well as the allocated addresses.
type Container struct {
	Name    string
	ID      string
	IPv4    string
	IPv6    string
	MAC     string
	Options nettypes.PerNetworkOptions
}

type State struct {
	Exists    bool
	IPv4      string
	Gateway4  string
	IPv6      string
	Gateway6  string
	Interface string
	Config    json.RawMessage // Complete network file, including unknown fields.
}

type snapshot struct {
	Network    string
	State      State
	Containers []Container
	Committed  bool
}

// Only the Podman implementation touches the machine; tests drive the transaction
// through this interface to cover failures at every destructive stage.
type backend interface {
	inspect(context.Context, string) (State, error)
	snapshot(context.Context, string, State) (*snapshot, error)
	disconnect(context.Context, string, string) error
	remove(context.Context, string) error
	create(context.Context, Options, json.RawMessage) error
	connect(context.Context, string, Container, bool) error
	load(string) (*snapshot, error)
	save(*snapshot) error
	clear(string) error
}

var networkLocks sync.Map

func networkLock(name string) *sync.Mutex {
	mu, _ := networkLocks.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// Rebuild persists recovery data before disconnecting anything. Once started, the
// operation has its own deadline and is not cancelled when the HTTP client leaves.
func Rebuild(ctx context.Context, want Options) (*Result, error) {
	if !nettypes.NameRegex.MatchString(want.Network) {
		return nil, fmt.Errorf("invalid network name %q", want.Network)
	}
	mu := networkLock(want.Network)
	mu.Lock()
	defer mu.Unlock()
	return rebuild(ctx, want, &podmanBackend{dir: networkDir})
}

// RecoverPending restores an interrupted migration before normal Agent startup.
// A completed transaction only needs its journal removed.
func RecoverPending(ctx context.Context, network string) error {
	if !nettypes.NameRegex.MatchString(network) {
		return fmt.Errorf("invalid network name %q", network)
	}
	mu := networkLock(network)
	mu.Lock()
	defer mu.Unlock()
	return recoverPending(ctx, network, &podmanBackend{dir: networkDir})
}

func recoverPending(ctx context.Context, network string, b backend) error {
	s, err := b.load(network)
	if err != nil || s == nil {
		return err
	}
	if s.Network != network {
		return fmt.Errorf("migration journal does not match network %q", network)
	}
	if !s.Committed {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
		defer cancel()
		if err := restore(rctx, s, b); err != nil {
			return fmt.Errorf("恢复上次中断的网络迁移失败（恢复快照已保留）：%w", err)
		}
		log.Printf("[NetMigrate] restored interrupted migration of %s", network)
	}
	return b.clear(network)
}

func rebuild(ctx context.Context, want Options, b backend) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := recoverPending(ctx, want.Network, b); err != nil {
		return nil, err
	}
	cur, err := b.inspect(ctx, want.Network)
	if err != nil {
		return nil, err
	}
	if cur.Exists && sameShape(cur, want) {
		return &Result{}, nil
	}
	if err := validateChange(cur, want); err != nil {
		return nil, err
	}
	if cur.Interface != "" && want.Interface == "" {
		want.Interface = cur.Interface
	}
	s, err := b.snapshot(ctx, want.Network, cur)
	if err != nil {
		return nil, err
	}
	targetConfig, err := targetConfig(s.State.Config, want)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.save(s); err != nil {
		return nil, fmt.Errorf("保存网络恢复快照失败，未修改网络：%w", err)
	}

	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	start := time.Now()
	fail := func(cause error) (*Result, error) {
		// Never reuse the failed operation's deadline for recovery.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
		defer rcancel()
		if err := restore(rctx, s, b); err != nil {
			return nil, errors.Join(cause, fmt.Errorf("恢复原网络失败（恢复快照已保留）：%w", err))
		}
		return nil, errors.Join(cause, b.clear(want.Network))
	}
	if cur.Exists {
		for _, c := range s.Containers {
			if err := b.disconnect(opCtx, want.Network, c.Name); err != nil {
				return fail(fmt.Errorf("容器 %s 脱离网络失败：%w", c.Name, err))
			}
		}
		if err := b.remove(opCtx, want.Network); err != nil {
			return fail(fmt.Errorf("删除网络失败：%w", err))
		}
	}
	if err := b.create(opCtx, want, targetConfig); err != nil {
		return fail(fmt.Errorf("创建网络失败：%w", err))
	}
	if err := reattachAll(opCtx, want.Network, s.Containers, want.IPv6 != "", b); err != nil {
		return fail(err)
	}
	// 网络重建后 netavark 会重写容器内 /etc/resolv.conf；新网络是 --disable-dns
	// （dns_enabled=false）时它直接把文件清空，容器进程没重启、不会自愈，
	// 表现为容器内 v4/v6 DNS 一起失效。这里按容器的持久 DNS 配置写回。
	restoreContainerDNS(opCtx, s.Containers)
	// A crash between commit and journal deletion must not undo a finished switch.
	s.Committed = true
	if err := b.save(s); err != nil {
		s.Committed = false
		return fail(fmt.Errorf("提交网络迁移失败：%w", err))
	}
	if err := b.clear(want.Network); err != nil {
		return nil, fmt.Errorf("网络迁移已完成，但清理恢复快照失败：%w", err)
	}
	res := &Result{Containers: len(s.Containers), Downtime: time.Since(start), Rebuilt: true}
	log.Printf("[NetMigrate] rebuilt %s: IPv4=%s IPv6=%s, restored %d containers in %s",
		want.Network, want.IPv4, want.IPv6, res.Containers, res.Downtime.Round(time.Second))
	return res, nil
}

func validateChange(cur State, want Options) error {
	for _, f := range []struct {
		cidr, gateway string
		v6            bool
	}{
		{want.IPv4, want.Gateway4, false}, {want.IPv6, want.Gateway6, true},
	} {
		if f.cidr == "" {
			if f.gateway != "" {
				return fmt.Errorf("gateway requires a subnet")
			}
			continue
		}
		ip, subnet, err := net.ParseCIDR(f.cidr)
		if err != nil || (ip.To4() == nil) != f.v6 {
			return fmt.Errorf("invalid subnet %q", f.cidr)
		}
		if f.gateway != "" && !subnet.Contains(net.ParseIP(f.gateway)) {
			return fmt.Errorf("gateway %s is outside subnet %s", f.gateway, f.cidr)
		}
	}
	if want.IPv4 == "" {
		return fmt.Errorf("IPv4 subnet is required")
	}
	if cur.Exists && cur.IPv4 != want.IPv4 {
		return fmt.Errorf("不支持更换现有 IPv4 子网，未修改网络")
	}
	if cur.Exists && cur.IPv6 != "" && want.IPv6 != "" && cur.IPv6 != want.IPv6 {
		return fmt.Errorf("不支持更换现有 IPv6 子网（subnet → snat），未修改网络")
	}
	return nil
}

func stateOptions(name string, s State) Options {
	return Options{Network: name, Interface: s.Interface, IPv4: s.IPv4,
		Gateway4: s.Gateway4, IPv6: s.IPv6, Gateway6: s.Gateway6}
}

func restore(ctx context.Context, s *snapshot, b backend) error {
	cur, err := b.inspect(ctx, s.Network)
	if err != nil {
		return err
	}
	// A disconnect/remove failure may have left the original network intact.
	// Reattach only missing bindings instead of interrupting healthy containers.
	if s.State.Exists && cur.Exists && sameShape(cur, stateOptions(s.Network, s.State)) && sameNetworkID(cur.Config, s.State.Config) {
		return reattachAll(ctx, s.Network, s.Containers, s.State.IPv6 != "", b)
	}
	if cur.Exists {
		var errs []error
		for _, c := range s.Containers {
			if err := b.disconnect(ctx, s.Network, c.Name); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		if err := b.remove(ctx, s.Network); err != nil {
			return err
		}
	}
	if !s.State.Exists {
		return nil
	}
	if err := b.create(ctx, stateOptions(s.Network, s.State), s.State.Config); err != nil {
		return err
	}
	return reattachAll(ctx, s.Network, s.Containers, s.State.IPv6 != "", b)
}

func sameNetworkID(a, b json.RawMessage) bool {
	var x, y struct {
		ID string `json:"id"`
	}
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && x.ID != "" && x.ID == y.ID
}

func reattachAll(ctx context.Context, network string, cs []Container, withV6 bool, b backend) error {
	var errs []error
	for _, c := range cs {
		if err := b.connect(ctx, network, c, withV6); err != nil {
			errs = append(errs, fmt.Errorf("容器 %s 接回网络失败：%w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// restoreContainerDNS 把容器的 /etc/resolv.conf 按创建时的 DNS 配置写回。
//
// netavark 在容器加入新网络时会重写 resolv.conf；网络是 --disable-dns 时写出来
// 是空的。容器进程不重启就不会自己恢复，容器内所有 DNS 查询随即失败。
// 这里直接写 ResolvConfPath（podman inspect 暴露的宿主机侧文件路径），
// 不重启容器、不影响已经建立的连接。
//
// 只处理显式配置过 DNS 的容器：没配的用宿主 resolv.conf，不该被我们覆盖。
func restoreContainerDNS(ctx context.Context, cs []Container) {
	for _, c := range cs {
		if c.ID == "" {
			continue
		}
		out, err := run(ctx, "podman", "inspect", c.ID, "--format", "{{json .HostConfig.Dns}}")
		if err != nil {
			continue
		}
		var dns []string
		if json.Unmarshal([]byte(strings.TrimSpace(out)), &dns) != nil || len(dns) == 0 {
			continue
		}
		pout, err := run(ctx, "podman", "inspect", c.ID, "--format", "{{.ResolvConfPath}}")
		if err != nil {
			continue
		}
		path := strings.TrimSpace(pout)
		if path == "" {
			continue
		}
		var b strings.Builder
		for _, s := range dns {
			if s = strings.TrimSpace(s); s != "" {
				b.WriteString("nameserver " + s + "\n")
			}
		}
		if b.Len() == 0 {
			continue
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			log.Printf("[NetMigrate] 写回 %s 的 resolv.conf 失败: %v", c.Name, err)
			continue
		}
		log.Printf("[NetMigrate] 已恢复 %s 的 DNS 配置（%d 个 nameserver）", c.Name, len(dns))
	}
}

func createArgs(o Options) []string {
	args := []string{"--driver=bridge", "--disable-dns"}
	if o.Interface != "" {
		args = append(args, "--interface-name", o.Interface)
	}
	if o.IPv4 != "" {
		args = append(args, "--subnet", o.IPv4)
		if o.Gateway4 != "" {
			args = append(args, "--gateway", o.Gateway4)
		}
	}
	if o.IPv6 != "" {
		args = append(args, "--ipv6", "--subnet", o.IPv6)
		if o.Gateway6 != "" {
			args = append(args, "--gateway", o.Gateway6)
		}
	}
	return append(args, o.Network)
}

func sameShape(cur State, want Options) bool {
	return cur.Exists && cur.IPv4 == want.IPv4 && cur.IPv6 == want.IPv6 &&
		(want.Gateway4 == "" || cur.Gateway4 == want.Gateway4) &&
		(want.Gateway6 == "" || cur.Gateway6 == want.Gateway6) &&
		(want.Interface == "" || cur.Interface == want.Interface)
}

func Inspect(ctx context.Context, name string) (State, error) {
	out, err := run(ctx, "podman", "network", "inspect", name)
	if err != nil {
		if strings.Contains(strings.ToLower(out), "not found") {
			return State{}, nil
		}
		return State{}, fmt.Errorf("读取网络 %s 失败：%w（%s）", name, err, strings.TrimSpace(out))
	}
	var parsed []json.RawMessage
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || len(parsed) != 1 {
		return State{}, fmt.Errorf("invalid network inspect response for %s", name)
	}
	return parseState(parsed[0])
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}
