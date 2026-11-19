//go:build containers_image_openpgp

package podman

import "testing"

func TestParsePrimaryIface(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{
			name: "eth0 优先",
			out: "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN\n" +
				"2: eth0@if26: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1450 qdisc noqueue state UP\n" +
				"3: tun0: <POINTOPOINT,MULTICAST,NOARP> mtu 1500 qdisc fifo state DOWN\n",
			want: "eth0",
		},
		{
			name: "无 eth0 时取第一个非 lo",
			out: "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN\n" +
				"5: ens4: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP\n",
			want: "ens4",
		},
		{
			name: "无 eth0 时优先带 @if 的 veth 而非租户自建接口",
			out: "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN\n" +
				"3: tun0: <POINTOPOINT,MULTICAST,NOARP> mtu 1500 qdisc fifo state UP\n" +
				"8: podman0@if33: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1450 qdisc noqueue state UP\n",
			want: "podman0",
		},
		{
			name:    "只有 lo 报错",
			out:     "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN\n",
			wantErr: true,
		},
		{
			name:    "空输出报错",
			out:     "",
			wantErr: true,
		},
		{
			name: "无 @if 后缀的裸接口名",
			out:  "2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1450 qdisc noqueue state UP\n",
			want: "eth0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePrimaryIface(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got iface=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}
