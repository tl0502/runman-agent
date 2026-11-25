package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/monitor"
	"runman-agent/proto/agent"
)

type reviewHeartbeatVM struct{ manager.VMManager }

func (reviewHeartbeatVM) ListVMs(context.Context) ([]*agent.VMSummary, error) {
	return []*agent.VMSummary{{VmId: "review", Ips: []string{"10.91.0.2", "fd91:cafe:cafe:10::2"}}}, nil
}
func (reviewHeartbeatVM) GetSupportedImages(context.Context) ([]*agent.OSImageInfo, error) {
	return nil, nil
}

type reviewHeartbeatStream struct {
	agent.AgentGateway_ConnectClient
	captured *agent.AgentEnvelope
}

func (s *reviewHeartbeatStream) Send(e *agent.AgentEnvelope) error { s.captured = e; return nil }

func TestHeartbeatUsesSharedIPv6Entry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if e := os.WriteFile(path, []byte(`{"virt_type":"podman","ipv6_mode":"snat","public_ipv6":"2001:db8:1::10"}`), 0600); e != nil {
		t.Fatal(e)
	}
	cfg, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	database, e := db.Init(filepath.Join(dir, "db.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	a := &Agent{cfg: cfg, db: database, mgr: reviewHeartbeatVM{}, hostMon: monitor.NewHostMonitor(), config: cfg.Get(), entryIPv6: "2001:db8:1::20",
		hostHasIPv6: func(string) bool { return true }}
	s := &reviewHeartbeatStream{}
	a.sendHeartbeat(s)
	if s.captured == nil || s.captured.GetHeartbeat() == nil {
		t.Fatal("heartbeat was not emitted")
	}
	hb := s.captured.GetHeartbeat()
	t.Logf("configured/listening IPv6=%s, advertised entry_ipv6=%s, VM ips=%v", cfg.Get().EgressIPv6(), hb.EntryIpv6, hb.Vms[0].Ips)
	if hb.EntryIpv6 != cfg.Get().EgressIPv6() {
		t.Errorf("platform advertises %s while shared IPv6 forwards bind to %s", hb.EntryIpv6, cfg.Get().EgressIPv6())
	}
}

func TestHeartbeatManualEntryIPv6(t *testing.T) {
	// 面板「入口 IPv6」手填的值只影响上报，不改变共享出口/监听的绑定地址。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"virt_type":"podman","ipv6_mode":"snat","public_ipv6":"2001:db8:1::10","host_ipv6":"2001:db8:9::77"}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: cfg, db: database, mgr: reviewHeartbeatVM{}, hostMon: monitor.NewHostMonitor(), config: cfg.Get(), entryIPv6: "2001:db8:1::20",
		hostHasIPv6: func(string) bool { return true }}
	s := &reviewHeartbeatStream{}
	a.sendHeartbeat(s)
	if s.captured == nil || s.captured.GetHeartbeat() == nil {
		t.Fatal("heartbeat was not emitted")
	}
	if got := s.captured.GetHeartbeat().EntryIpv6; got != "2001:db8:9::77" {
		t.Errorf("entry_ipv6 = %s, want manual 2001:db8:9::77 (binding stays %s)", got, cfg.Get().EgressIPv6())
	}
}

func TestLegacyPortForwardMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	_, e = d.Exec(`CREATE TABLE port_forwards (protocol TEXT, host_port INTEGER, vm_id TEXT, guest_port INTEGER, description TEXT, PRIMARY KEY(protocol,host_port)); INSERT INTO port_forwards VALUES ('tcp',22222,'legacy',22,'ssh')`)
	if e != nil {
		t.Fatal(e)
	}
	d.Close()
	migrated, e := db.Init(path)
	if e != nil {
		t.Fatal(e)
	}
	rows, e := migrated.ListPortForwards("legacy")
	if e != nil || len(rows) != 1 {
		t.Fatalf("legacy rows were not preserved: %v %v", rows, e)
	}
	if rows[0].BindFamily != 0 || rows[0].GuestPort != 22 {
		t.Fatalf("legacy semantics changed: %+v", rows[0])
	}
	rows[0].BindFamily = 2
	if e = migrated.SavePortForward(rows[0]); e != nil {
		t.Fatal(e)
	}
	rows, e = migrated.ListPortForwards("legacy")
	if e != nil || len(rows) != 1 || rows[0].BindFamily != 2 {
		t.Fatalf("new address family did not persist: %+v %v", rows, e)
	}
	t.Log("legacy row preserved with bind_family=0; dual-stack value persists after migration")
}

func TestHeartbeatPreservesOtherIPv6Modes(t *testing.T) {
	for _, tc := range []struct {
		name, virt, mode, public, addr, want string
		// hostUp=false 模拟配置的共享 IPv6 已不在网卡上（实例重建/地址漂移）。
		hostUp bool
	}{
		{"shared fallback", "podman", "snat", "", "2001:db8:1::11", "2001:db8:1::11", true},
		{"shared stale", "podman", "snat", "", "2001:db8:1::11", "2001:db8:1::20", false},
		{"subnet", "podman", "subnet", "2001:db8:1::10", "", "2001:db8:1::20", true},
		{"disabled", "podman", "none", "2001:db8:1::10", "", "2001:db8:1::20", true},
		{"other backend", "incus", "snat", "2001:db8:1::10", "", "2001:db8:1::20", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(`{"virt_type":"podman"}`), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Update(func(c *config.Config) {
				c.VirtType = tc.virt
				c.IPv6Mode = tc.mode
				c.PublicIPv6 = tc.public
				c.IPv6Addr = tc.addr
			}); err != nil {
				t.Fatal(err)
			}
			database, err := db.Init(filepath.Join(dir, "agent.db"))
			if err != nil {
				t.Fatal(err)
			}
			a := &Agent{cfg: cfg, db: database, mgr: reviewHeartbeatVM{}, hostMon: monitor.NewHostMonitor(), config: cfg.Get(), entryIPv6: "2001:db8:1::20",
				hostHasIPv6: func(string) bool { return tc.hostUp }}
			s := &reviewHeartbeatStream{}
			a.sendHeartbeat(s)
			if s.captured == nil || s.captured.GetHeartbeat() == nil || s.captured.GetHeartbeat().EntryIpv6 != tc.want {
				t.Fatalf("heartbeat entry differs in %s: %+v", tc.name, s.captured)
			}
		})
	}
}
