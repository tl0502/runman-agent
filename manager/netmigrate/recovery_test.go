package netmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	nettypes "go.podman.io/common/libnetwork/types"
)

const originalNetwork = `{"name":"test","id":"original","driver":"bridge","network_interface":"podman1","subnets":[{"subnet":"10.91.0.0/20","gateway":"10.91.0.1","lease_range":{"start_ip":"10.91.0.10","end_ip":"10.91.0.200"}}],"ipv6_enabled":false,"internal":false,"dns_enabled":true,"network_dns_servers":["1.1.1.1"],"labels":{"owner":"test"},"options":{"mtu":"1400","outbound_addr4":"192.0.2.1"},"ipam_options":{"driver":"host-local"},"future_field":{"keep":true}}`

type testBackend struct {
	state         State
	containers    []Container
	bound         map[string]Container
	journal       *snapshot
	failures      map[string]int
	calls         []string
	afterRemove   func()
	createContext context.Context
}

func clone[T any](v T) T {
	data, _ := json.Marshal(v)
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

func fixture(t *testing.T) (*testBackend, Options) {
	t.Helper()
	st, err := parseState(json.RawMessage(originalNetwork))
	if err != nil {
		t.Fatal(err)
	}
	cs := []Container{
		{Name: "running", IPv4: "10.91.0.11", Options: nettypes.PerNetworkOptions{StaticIPs: []net.IP{net.ParseIP("10.91.0.11")}, Aliases: []string{"download"}, InterfaceName: "eth0", Options: map[string]string{"bandwidth_rate": "2000000", "bandwidth_burst": "1000000", "bandwidth_latency": "50"}}},
		{Name: "stopped", IPv4: "10.91.0.42", Options: nettypes.PerNetworkOptions{StaticIPs: []net.IP{net.ParseIP("10.91.0.42")}, InterfaceName: "eth1", Aliases: []string{"offline"}}},
	}
	b := &testBackend{state: st, containers: cs, bound: map[string]Container{}, failures: map[string]int{}}
	for _, c := range cs {
		b.bound[c.Name] = clone(c)
	}
	return b, Options{Network: "test", IPv4: "10.91.0.0/20", Gateway4: "10.91.0.1", IPv6: "fd91:cafe:cafe:10::/64", Gateway6: "fd91:cafe:cafe:10::1"}
}

func (b *testBackend) step(ctx context.Context, name string) error {
	b.calls = append(b.calls, name)
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.failures[name] > 0 {
		b.failures[name]--
		return fmt.Errorf("injected %s failure", name)
	}
	return nil
}

func (b *testBackend) inspect(ctx context.Context, _ string) (State, error) {
	return clone(b.state), ctx.Err()
}
func (b *testBackend) snapshot(ctx context.Context, name string, _ State) (*snapshot, error) {
	if err := b.step(ctx, "snapshot"); err != nil {
		return nil, err
	}
	return &snapshot{Network: name, State: clone(b.state), Containers: clone(b.containers)}, nil
}
func (b *testBackend) disconnect(ctx context.Context, _, name string) error {
	if err := b.step(ctx, "disconnect:"+name); err != nil {
		return err
	}
	delete(b.bound, name)
	return nil
}
func (b *testBackend) remove(ctx context.Context, _ string) error {
	if err := b.step(ctx, "remove"); err != nil {
		return err
	}
	if len(b.bound) > 0 {
		return errors.New("network in use")
	}
	b.state = State{}
	if b.afterRemove != nil {
		f := b.afterRemove
		b.afterRemove = nil
		f()
	}
	return nil
}
func (b *testBackend) create(ctx context.Context, want Options, raw json.RawMessage) error {
	b.createContext = ctx
	if err := b.step(ctx, "create"); err != nil {
		return err
	}
	if b.state.Exists {
		return errors.New("network already exists")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc["id"]) == 0 {
		doc["id"] = json.RawMessage(`"rebuilt"`)
	}
	data, _ := json.Marshal(doc)
	st, err := parseState(data)
	if err != nil {
		return err
	}
	b.state = st
	return nil
}
func (b *testBackend) connect(ctx context.Context, _ string, c Container, withV6 bool) error {
	if err := b.step(ctx, "connect:"+c.Name); err != nil {
		return err
	}
	if !b.state.Exists {
		return errors.New("network missing")
	}
	if _, exists := b.bound[c.Name]; exists {
		return nil
	}
	c = clone(c)
	if !withV6 {
		ips := c.Options.StaticIPs[:0]
		for _, ip := range c.Options.StaticIPs {
			if ip.To4() != nil {
				ips = append(ips, ip)
			}
		}
		c.Options.StaticIPs = ips
	}
	b.bound[c.Name] = c
	return nil
}
func (b *testBackend) load(string) (*snapshot, error) {
	if b.journal == nil {
		return nil, nil
	}
	s := clone(*b.journal)
	return &s, nil
}
func (b *testBackend) save(s *snapshot) error {
	if err := b.step(context.Background(), "save"); err != nil {
		return err
	}
	out := clone(*s)
	b.journal = &out
	return nil
}
func (b *testBackend) clear(string) error { b.journal = nil; return nil }

func assertRestored(t *testing.T, b *testBackend) {
	t.Helper()
	if !sameNetworkID(b.state.Config, json.RawMessage(originalNetwork)) || b.state.IPv6 != "" {
		t.Fatalf("original network not restored: %+v", b.state)
	}
	for _, c := range b.containers {
		if got, ok := b.bound[c.Name]; !ok || !reflect.DeepEqual(got.Options, c.Options) {
			t.Fatalf("binding %s lost configuration: %+v", c.Name, got)
		}
	}
	if b.journal != nil {
		t.Fatal("successful recovery left an active journal")
	}
}

func TestRebuildRecoversEveryFailureStage(t *testing.T) {
	for _, stage := range []string{"disconnect:stopped", "remove", "create", "connect:stopped"} {
		t.Run(stage, func(t *testing.T) {
			b, want := fixture(t)
			b.failures[stage] = 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b.afterRemove = cancel
			if _, err := rebuild(ctx, want, b); err == nil {
				t.Fatal("failure was reported as success")
			}
			assertRestored(t, b)
		})
	}
}

func TestRebuildFinishesAfterCallerCancellation(t *testing.T) {
	b, want := fixture(t)
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), "test-request", "retained"))
	defer cancel()
	b.afterRemove = cancel
	res, err := rebuild(ctx, want, b)
	if err != nil || !res.Rebuilt || res.Containers != 2 {
		t.Fatalf("cancelled caller interrupted migration: %+v %v", res, err)
	}
	if ctx.Err() == nil {
		t.Fatal("cancellation was not injected")
	}
	if b.createContext.Value("test-request") != "retained" {
		t.Fatal("request context values were lost")
	}
	for _, c := range b.containers {
		if !reflect.DeepEqual(b.bound[c.Name].Options, c.Options) {
			t.Fatalf("network options changed: %+v", b.bound[c.Name])
		}
	}
	if b.journal != nil {
		t.Fatal("journal not cleaned")
	}
}

func TestRebuildRequiresRecoverySnapshotBeforeMutation(t *testing.T) {
	b, want := fixture(t)
	b.failures["save"] = 1
	if _, err := rebuild(t.Context(), want, b); err == nil {
		t.Fatal("snapshot write failure ignored")
	}
	assertRestored(t, b)
	if strings.Contains(strings.Join(b.calls, " "), "disconnect:") {
		t.Fatal("network changed without durable recovery data")
	}
}

func TestRebuildRetainsJournalWhenRecoveryFails(t *testing.T) {
	b, want := fixture(t)
	b.failures["create"] = 2
	if _, err := rebuild(t.Context(), want, b); err == nil || !strings.Contains(err.Error(), "恢复快照已保留") {
		t.Fatalf("rollback failure not surfaced: %v", err)
	}
	if b.journal == nil || b.journal.Committed {
		t.Fatal("recovery material was discarded")
	}
	if err := recoverPending(t.Context(), "test", b); err != nil {
		t.Fatal(err)
	}
	assertRestored(t, b)
}

func TestCompletedJournalDoesNotRollBackCommittedNetwork(t *testing.T) {
	b, _ := fixture(t)
	b.journal = &snapshot{Network: "test", Committed: true}
	if err := recoverPending(t.Context(), "test", b); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 0 || b.journal != nil {
		t.Fatal("completed migration triggered destructive recovery")
	}
}

func TestSubnetToSharedRejectedBeforeDisconnect(t *testing.T) {
	b, want := fixture(t)
	b.state.IPv6 = "2001:db8:f001::/64"
	if _, err := rebuild(t.Context(), want, b); err == nil || !strings.Contains(err.Error(), "subnet → snat") {
		t.Fatalf("unsafe conversion accepted: %v", err)
	}
	if len(b.calls) != 0 || len(b.bound) != 2 {
		t.Fatal("rejected conversion modified the network")
	}
}

func TestTargetConfigPreservesNetworkMetadata(t *testing.T) {
	_, want := fixture(t)
	raw, err := targetConfig(json.RawMessage(originalNetwork), want)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	json.Unmarshal([]byte(originalNetwork), &before)
	json.Unmarshal(raw, &after)
	for _, key := range []string{"driver", "dns_enabled", "internal", "network_dns_servers", "labels", "options", "ipam_options", "future_field"} {
		var a, b any
		json.Unmarshal(before[key], &a)
		json.Unmarshal(after[key], &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s changed: %s", key, after[key])
		}
	}
	var subnets []map[string]json.RawMessage
	json.Unmarshal(after["subnets"], &subnets)
	if len(subnets) != 2 || len(subnets[0]["lease_range"]) == 0 {
		t.Fatal("IPv4 allocation range was discarded")
	}
	if _, ok := after["id"]; ok {
		t.Fatal("target reuses the old network identity")
	}
}
