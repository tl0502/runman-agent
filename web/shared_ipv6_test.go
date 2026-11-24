package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/netmigrate"
	"runman-agent/manager/portforward"
)

type testIPv6Egress struct{ err error }

func (e testIPv6Egress) Apply(context.Context) error { return e.err }

type testIPv6VM struct{ manager.VMManager }

func (testIPv6VM) GetVMLocalIP(context.Context, string) (string, error) { return "127.0.0.1", nil }

func TestSharedIPv6ResponseRequiresEgressSuccess(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		s := newConfigTestServer(t)
		s.egress = testIPv6Egress{err: errors.New("SNAT application failed")}
		rec := httptest.NewRecorder()
		s.finishSharedIPv6(t.Context(), rec, &netmigrate.Result{Rebuilt: true, Containers: 2}, enabled)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "SNAT application failed") || strings.Contains(rec.Body.String(), `"enabled":true`) {
			t.Fatalf("false success: HTTP %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestSharedIPv6ResponseRequiresListenerSuccess(t *testing.T) {
	s := newConfigTestServer(t)
	d, err := db.Init(filepath.Join(t.TempDir(), "forward.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.db = d
	if err := s.cfg.Update(func(c *config.Config) { c.VirtType = "podman"; c.IPv6Mode = "snat"; c.PublicIPv6 = ""; c.IPv6Addr = "" }); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SavePortForward(&db.PortForward{VMID: "pending", Protocol: "tcp", HostPort: 22222, GuestPort: 22, BindFamily: portforward.BindBoth}); err != nil {
		t.Fatal(err)
	}
	s.pf = portforward.New(testIPv6VM{}, s.db, s.cfg)
	s.egress = testIPv6Egress{}
	rec := httptest.NewRecorder()
	s.finishSharedIPv6(t.Context(), rec, &netmigrate.Result{}, true)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "shared IPv6 address is not configured") {
		t.Fatalf("listener error was hidden: HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestSharedIPv6SuccessResponse(t *testing.T) {
	s := newConfigTestServer(t)
	s.egress = testIPv6Egress{}
	rec := httptest.NewRecorder()
	s.finishSharedIPv6(t.Context(), rec, &netmigrate.Result{Rebuilt: true, Containers: 3, Downtime: 1500 * time.Millisecond}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) || !strings.Contains(rec.Body.String(), `"downtime_ms":1500`) {
		t.Fatalf("bad success response: HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestSharedIPv6RejectsSubnetModeBeforeNetworkCommands(t *testing.T) {
	s := newConfigTestServer(t)
	if err := s.cfg.Update(func(c *config.Config) { c.VirtType = "podman"; c.IPv6Mode = "subnet" }); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"enabled":true,"addr":"2001:db8::1"}`, `{"enabled":false}`} {
		rec := httptest.NewRecorder()
		s.handleShareIPv6(rec, httptest.NewRequest(http.MethodPost, "/api/ipv6/share", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "subnet") {
			t.Fatalf("unsafe mode accepted: HTTP %d %s", rec.Code, rec.Body.String())
		}
	}
	if s.cfg.Get().IPv6Mode != "subnet" {
		t.Fatal("rejected conversion changed configuration")
	}
}
