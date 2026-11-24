//go:build linux

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"runman-agent/config"
	"runman-agent/manager/egress"
)

// Run unprivileged on Deba1. The real root-owned network file must not be
// writable; the failed temporary-file write is checked before calling the API.
// Podman inspect is mocked and reports an already-matching network, so this
// test cannot rebuild any live network or execute any nft command.
func TestIntegrationShareIPv6SurfacesEgressFailure(t *testing.T) {
	if os.Getenv("RUNMAN_SHARED_IPV6_INTEGRATION") != "1" {
		t.Skip("requires opt-in unprivileged runtime environment")
	}
	if os.Geteuid() == 0 {
		t.Skip("must run unprivileged")
	}
	const networkDir = "/etc/containers/networks"
	if _, e := os.Stat(filepath.Join(networkDir, "narwhal-net.json")); e != nil {
		t.Skip(e)
	}
	if unix.Access(networkDir, unix.W_OK) == nil {
		t.Skip("network directory is writable; refusing mutation")
	}
	addrs := listHostIPv6()
	if len(addrs) == 0 {
		t.Skip("host has no global IPv6 address")
	}
	s := newConfigTestServer(t)
	if e := s.cfg.Update(func(c *config.Config) { c.VirtType = "podman"; c.PublicIPv4 = "192.0.2.123"; c.IPv6Mode = "none" }); e != nil {
		t.Fatal(e)
	}
	eg := egress.New(s.cfg)
	if e := eg.Apply(t.Context()); e == nil || !strings.Contains(e.Error(), "permission denied") {
		t.Fatalf("expected safe write rejection before nft execution, got %v", e)
	} else {
		t.Logf("confirmed egress failure: %v", e)
	}
	bin := t.TempDir()
	stub := `#!/bin/sh
if [ "$1" = network ] && [ "$2" = inspect ]; then
  printf '%s\n' '[{"network_interface":"podman1","subnets":[{"subnet":"10.91.0.0/20","gateway":"10.91.0.1"},{"subnet":"fd91:cafe:cafe:10::/64","gateway":"fd91:cafe:cafe:10::1"}]}]'
else
  exit 93
fi
`
	if e := os.WriteFile(filepath.Join(bin, "podman"), []byte(stub), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	s.SetEgress(eg)
	b, e := json.Marshal(map[string]any{"enabled": true, "addr": addrs[0]})
	if e != nil {
		t.Fatal(e)
	}
	rec := httptest.NewRecorder()
	s.handleShareIPv6(rec, httptest.NewRequest(http.MethodPost, "/api/ipv6/share", bytes.NewReader(b)))
	t.Logf("HTTP status=%d body=%s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "permission denied") || strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("API hid SNAT application failure: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if got := s.cfg.Get(); got.IPv6Mode != "snat" || got.PublicIPv6 != addrs[0] {
		t.Errorf("saved desired configuration was lost: mode=%s IPv6=%s", got.IPv6Mode, got.PublicIPv6)
	}
}
