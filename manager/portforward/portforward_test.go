package portforward

import (
	"reflect"
	"testing"
)

func TestFormatTargetAddress(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		port int
		want string
	}{
		{name: "IPv4", ip: "10.91.0.2", port: 22, want: "10.91.0.2:22"},
		{name: "IPv6", ip: "2001:db8:100::2", port: 22, want: "[2001:db8:100::2]:22"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTargetAddress(tc.ip, tc.port); got != tc.want {
				t.Fatalf("formatTargetAddress(%q, %d) = %q, want %q", tc.ip, tc.port, got, tc.want)
			}
		})
	}
}

// TestForwardPorts 验证按协议分组、去重、升序。
func TestForwardPorts(t *testing.T) {
	m := &Manager{mappings: map[string][]*Entry{
		"vm1": {
			{Protocol: "tcp", HostPort: 22001},
			{Protocol: "tcp", HostPort: 22001}, // 去重
			{Protocol: "tcp", HostPort: 49624},
			{Protocol: "udp", HostPort: 51820},
		},
		"vm2": {
			{Protocol: "tcp", HostPort: 8080},
		},
	}}
	got := m.ForwardPorts()
	want := map[string][]int{
		"tcp": {8080, 22001, 49624},
		"udp": {51820},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ForwardPorts() = %v, want %v", got, want)
	}
}

// TestForwardPortsEmpty 验证无规则时返回空 map（不 panic）。
func TestForwardPortsEmpty(t *testing.T) {
	m := &Manager{mappings: map[string][]*Entry{}}
	if got := m.ForwardPorts(); len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}
