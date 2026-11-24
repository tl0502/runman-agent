package portforward

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
)

type socketVM struct{ manager.VMManager }

func (socketVM) GetVMLocalIP(context.Context, string) (string, error) { return "127.0.0.1", nil }

func socketManager(t *testing.T, public4 string) (*Manager, *config.Manager, *db.DB) {
	t.Helper()
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	probe.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"virt_type":"podman","ipv6_mode":"snat","public_ipv6":"::1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) { c.PublicIPv4 = public4 }); err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(socketVM{}, database, cfg)
	t.Cleanup(func() { m.DeleteVM(context.Background(), "test") })
	return m, cfg, database
}

func echoTarget(t *testing.T, protocol string) int {
	t.Helper()
	if protocol == "tcp" {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); io.Copy(c, c) }()
			}
		}()
		return ln.Addr().(*net.TCPAddr).Port
	}
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 1024)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(b[:n], a)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func availablePort(t *testing.T, protocol string) int {
	t.Helper()
	if protocol == "tcp" {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		return port
	}
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return port
}

func assertEcho(t *testing.T, protocol, ip string, port int) {
	t.Helper()
	c, err := net.DialTimeout(protocol, net.JoinHostPort(ip, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	const payload = "forwarding-survived"
	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len(payload))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != payload {
		t.Fatalf("unexpected reply: %q", b)
	}
}

func TestDualStackWildcardTCPAndUDP(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			m, _, _ := socketManager(t, "")
			guest := echoTarget(t, protocol)
			port := availablePort(t, protocol)
			if err := m.AddMapping(t.Context(), "test", protocol, port, guest, "dual", BindBoth); err != nil {
				t.Fatal(err)
			}
			assertEcho(t, protocol, "127.0.0.1", port)
			assertEcho(t, protocol, "::1", port)
		})
	}
}

func TestFailedFamilyUpgradePreservesOldMapping(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			m, _, database := socketManager(t, "127.0.0.1")
			guest := echoTarget(t, protocol)
			port := availablePort(t, protocol)
			addr := net.JoinHostPort("::1", strconv.Itoa(port))
			var blocker io.Closer
			var err error
			if protocol == "tcp" {
				blocker, err = net.Listen("tcp6", addr)
			} else {
				blocker, err = net.ListenPacket("udp6", addr)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Close()
			if err := m.AddMapping(t.Context(), "test", protocol, port, guest, "original", BindV4); err != nil {
				t.Fatal(err)
			}
			assertEcho(t, protocol, "127.0.0.1", port)
			if err := m.AddMapping(t.Context(), "test", protocol, port, availablePort(t, protocol), "replacement", BindBoth); err == nil {
				t.Fatal("conflicting IPv6 bind succeeded")
			}
			assertEcho(t, protocol, "127.0.0.1", port)
			report := m.GetReport()
			if len(report) != 1 || report[0].BindFamily != BindV4 || report[0].GuestPort != guest || report[0].Description != "original" {
				t.Fatalf("old live mapping lost: %+v", report)
			}
			rows, err := database.ListPortForwards("test")
			if err != nil || len(rows) != 1 || rows[0].BindFamily != BindV4 || rows[0].GuestPort != guest || rows[0].Description != "original" {
				t.Fatalf("old DB rule lost: %+v %v", rows, err)
			}
		})
	}
}

func TestReloadSurfacesFailureAndKeepsOldListener(t *testing.T) {
	m, cfg, _ := socketManager(t, "127.0.0.1")
	guest := echoTarget(t, "tcp")
	port := availablePort(t, "tcp")
	if err := m.AddMapping(t.Context(), "test", "tcp", port, guest, "original", BindBoth); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) { c.PublicIPv6 = "2001:db8::99" }); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadAll(t.Context()); err == nil {
		t.Fatal("rebind failure was reported as success")
	}
	assertEcho(t, "tcp", "127.0.0.1", port)
	assertEcho(t, "tcp", "::1", port)
	if _, stale := m.staleTarget(t.Context(), "test", "tcp", port, guest); !stale {
		t.Fatal("reconciler cannot detect the old bind address")
	}
	if err := cfg.Update(func(c *config.Config) { c.PublicIPv6 = "::1" }); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadAll(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestReloadRetriesDatabaseRuleWithoutLiveMapping(t *testing.T) {
	m, _, database := socketManager(t, "127.0.0.1")
	guest := echoTarget(t, "tcp")
	port := availablePort(t, "tcp")
	if err := database.SavePortForward(&db.PortForward{VMID: "test", Protocol: "tcp", HostPort: port, GuestPort: guest, BindFamily: BindV6}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertEcho(t, "tcp", "::1", port)
}
