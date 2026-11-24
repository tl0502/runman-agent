package podman

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
)

func TestNetworkMigrationBlocksContainerStart(t *testing.T) {
	started := make(chan struct{}, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Libpod-API-Version", "5.4.2")
			w.Header().Set("API-Version", "1.41")
			w.Write([]byte("OK"))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/test/start") {
			started <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected Podman request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()
	ctx, err := bindings.NewConnection(context.Background(), "tcp://"+strings.TrimPrefix(api.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{ctx: ctx}
	unlock := m.LockNetworkMigration()
	held := true
	defer func() {
		if held {
			unlock()
		}
	}()
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() { close(ready); result <- m.StartVM(t.Context(), "test") }()
	<-ready
	select {
	case <-started:
		t.Fatal("container start ran during migration")
	case err := <-result:
		t.Fatalf("container start bypassed migration: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	held = false
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("container start did not resume after migration")
	}
	select {
	case <-started:
	default:
		t.Fatal("container start was not sent to Podman")
	}
}
