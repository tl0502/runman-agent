//go:build linux

package netmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/specgen"
	nettypes "go.podman.io/common/libnetwork/types"
)

// Opt-in root integration tests use only unique tmp-fix-netmigrate-* resources.
// Fault injection wraps real Podman operations; it never substitutes production
// executables, edits Podman's state DB, or alters host firewall configuration.
type faultBackend struct {
	backend
	stage, lastContainer string
	failed               bool
	createFailures       int
	cancelCaller         context.CancelFunc
}

func (b *faultBackend) remove(ctx context.Context, name string) error {
	err := b.backend.remove(ctx, name)
	if err == nil && b.cancelCaller != nil {
		b.cancelCaller()
		b.cancelCaller = nil
	}
	return err
}
func (b *faultBackend) create(ctx context.Context, want Options, raw json.RawMessage) error {
	if b.stage == "recovery" && b.createFailures < 2 {
		b.createFailures++
		b.failed = true
		return errors.New("injected create and rollback failure")
	}
	if b.stage == "create" && !b.failed {
		b.failed = true
		return errors.New("injected create failure")
	}
	return b.backend.create(ctx, want, raw)
}
func (b *faultBackend) disconnect(ctx context.Context, network, name string) error {
	err := b.backend.disconnect(ctx, network, name)
	if err == nil && b.stage == "disconnect" && name == b.lastContainer && !b.failed {
		b.failed = true
		return errors.New("injected failure after disconnect")
	}
	return err
}
func (b *faultBackend) connect(ctx context.Context, network string, c Container, withV6 bool) error {
	err := b.backend.connect(ctx, network, c, withV6)
	if err == nil && b.stage == "connect" && c.Name == b.lastContainer && !b.failed {
		b.failed = true
		return errors.New("injected failure after reconnect")
	}
	return err
}

func integrationCommand(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := run(ctx, "podman", args...)
	if err != nil {
		t.Fatalf("podman %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(out)
}

func integrationQdisc(t *testing.T, name string) string {
	t.Helper()
	out := integrationCommand(t, t.Context(), "exec", name, "ip", "-o", "link")
	matches := regexp.MustCompile(`eth0@if([0-9]+)`).FindStringSubmatch(out)
	if len(matches) != 2 {
		t.Fatalf("cannot find host veth: %s", out)
	}
	out, err := run(t.Context(), "ip", "-j", "link")
	if err != nil {
		t.Fatal(err)
	}
	var links []struct {
		IfIndex int
		IfName  string
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if fmt.Sprint(link.IfIndex) == matches[1] {
			out, err := run(t.Context(), "tc", "qdisc", "show", "dev", link.IfName)
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(out)
		}
	}
	t.Fatal("host veth not found")
	return ""
}

func TestIntegrationMigration(t *testing.T) {
	if os.Getenv("RUNMAN_NETMIGRATE_INTEGRATION") != "1" || os.Geteuid() != 0 {
		t.Skip("requires opt-in root Podman integration environment")
	}
	for _, stage := range []string{"success", "cancel", "create", "connect", "disconnect", "recovery"} {
		t.Run(stage, func(t *testing.T) {
			name := fmt.Sprintf("tmp-fix-netmigrate-%x", time.Now().UnixNano())
			running, stopped := name+"-running", name+"-stopped"
			b := &podmanBackend{dir: networkDir}
			want := Options{Network: name, IPv4: "10.249.130.0/24", Gateway4: "10.249.130.1", IPv6: "fd42:feed:130::/64", Gateway6: "fd42:feed:130::1"}
			integrationCommand(t, t.Context(), "network", "create", "--disable-dns", "--opt", "mtu=1400", "--label", "runman-fix=20261124", "--subnet", want.IPv4, "--gateway", want.Gateway4, name)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if out, err := run(ctx, "podman", "rm", "-f", "--time=0", running, stopped); err != nil {
					t.Logf("cleanup containers: %v %s", err, out)
				}
				if out, err := run(ctx, "podman", "network", "rm", name); err != nil {
					t.Errorf("cleanup network: %v %s", err, out)
				}
				if err := b.clear(name); err != nil {
					t.Error(err)
				}
			})
			path := filepath.Join(networkDir, name+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			json.Unmarshal(raw, &doc)
			doc["future_marker"] = json.RawMessage(`{"keep":true}`)
			raw, _ = json.Marshal(doc)
			if err := atomicWrite(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			apiCtx, err := bindings.NewConnection(t.Context(), podmanSocket)
			if err != nil {
				t.Fatal(err)
			}
			image := os.Getenv("RUNMAN_NETMIGRATE_IMAGE")
			if image == "" {
				image = "docker.io/library/python:3.13-alpine"
			}
			sp := specgen.NewSpecGenerator(image, false)
			sp.Name = running
			sp.Entrypoint = []string{"sleep"}
			sp.Command = []string{"600"}
			sp.Networks = map[string]nettypes.PerNetworkOptions{name: {StaticIPs: []net.IP{net.ParseIP("10.249.130.11")}, Aliases: []string{"migration-alias"}, InterfaceName: "eth0", Options: map[string]string{"bandwidth_rate": "2000000", "bandwidth_burst": "1000000", "bandwidth_latency": "50"}}}
			created, err := containers.CreateWithSpec(apiCtx, sp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := containers.Start(apiCtx, created.ID, nil); err != nil {
				t.Fatal(err)
			}
			integrationCommand(t, t.Context(), "create", "--name", stopped, "--network", name+":ip=10.249.130.42", "--entrypoint", "sleep", image, "600")
			st, err := b.inspect(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			before, err := b.snapshot(t.Context(), name, st)
			if err != nil {
				t.Fatal(err)
			}
			beforeQdisc := integrationQdisc(t, running)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fb := &faultBackend{backend: b, stage: stage, lastContainer: before.Containers[len(before.Containers)-1].Name}
			if stage == "cancel" {
				fb.cancelCaller = cancel
			}
			res, err := rebuild(ctx, want, fb)
			if stage == "success" || stage == "cancel" {
				if err != nil || res == nil || !res.Rebuilt || res.Containers != 2 {
					t.Fatalf("migration failed: %+v %v", res, err)
				}
			} else if err == nil || !fb.failed {
				t.Fatalf("failure injection did not trigger recovery: %+v %v", res, err)
			}
			if stage == "recovery" {
				if pending, loadErr := b.load(name); loadErr != nil || pending == nil || pending.Committed {
					t.Fatalf("failed rollback lost recovery material: %+v %v", pending, loadErr)
				}
				if err := RecoverPending(t.Context(), name); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "cancel" && ctx.Err() == nil {
				t.Fatal("caller cancellation was not injected")
			}
			afterState, err := b.inspect(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			after, err := b.snapshot(t.Context(), name, afterState)
			if err != nil {
				t.Fatal(err)
			}
			var beforeDoc, afterDoc map[string]any
			json.Unmarshal(before.State.Config, &beforeDoc)
			json.Unmarshal(after.State.Config, &afterDoc)
			for _, key := range []string{"options", "labels", "ipam_options", "dns_enabled", "internal", "future_marker"} {
				if !reflect.DeepEqual(beforeDoc[key], afterDoc[key]) {
					t.Fatalf("network %s lost: before=%v after=%v", key, beforeDoc[key], afterDoc[key])
				}
			}
			if stage != "success" && stage != "cancel" {
				if !reflect.DeepEqual(beforeDoc, afterDoc) {
					t.Fatal("rollback did not restore the complete original network document")
				}
			}
			for _, original := range before.Containers {
				var actual *Container
				for i := range after.Containers {
					if after.Containers[i].Name == original.Name {
						actual = &after.Containers[i]
						break
					}
				}
				if actual == nil || actual.IPv4 != original.IPv4 || !reflect.DeepEqual(actual.Options.Options, original.Options.Options) || !reflect.DeepEqual(actual.Options.Aliases, original.Options.Aliases) || actual.Options.InterfaceName != original.Options.InterfaceName {
					t.Fatalf("container network configuration lost: before=%+v after=%+v", original, actual)
				}
			}
			afterQdisc := integrationQdisc(t, running)
			if !strings.Contains(beforeQdisc, "rate 2Mbit") || !strings.Contains(afterQdisc, "rate 2Mbit") {
				t.Fatalf("download bandwidth lost: before=%s after=%s", beforeQdisc, afterQdisc)
			}
			integrationCommand(t, t.Context(), "start", stopped)
			ip := integrationCommand(t, t.Context(), "inspect", stopped, "--format", fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, name))
			if ip != "10.249.130.42" {
				t.Fatalf("stopped static IP changed to %s", ip)
			}
			if pending, err := b.load(name); err != nil || pending != nil {
				t.Fatalf("journal left pending: %+v %v", pending, err)
			}
			t.Logf("%s: download=%s; stopped IPv4=%s; network metadata and aliases preserved", stage, afterQdisc, ip)
		})
	}
}
