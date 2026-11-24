package netmigrate

import (
	"strings"
	"testing"
)

// createArgs 返回的是子命令之后的参数，不能再包含 "create"。
// 线上踩过：多带一个 create 会让 podman 收到两个位置参数，
// 结果是旧网络已经删掉、新网络建不起来，容器全部断网。
func TestCreateArgsHasNoSubcommand(t *testing.T) {
	got := createArgs(Options{
		Network:   "narwhal-net",
		Interface: "podman1",
		IPv4:      "10.91.0.0/20",
		Gateway4:  "10.91.0.1",
		IPv6:      "fd91:cafe:cafe:10::/64",
		Gateway6:  "fd91:cafe:cafe:10::1",
	})

	for _, a := range got {
		if a == "create" || a == "network" {
			t.Fatalf("参数里不应包含子命令 %q，实际：%v", a, got)
		}
	}

	if last := got[len(got)-1]; last != "narwhal-net" {
		t.Fatalf("最后一个参数必须是网络名，实际 %q", last)
	}

	joined := strings.Join(got, " ")
	for _, want := range []string{"--driver=bridge", "--disable-dns", "--interface-name podman1",
		"--subnet 10.91.0.0/20", "--gateway 10.91.0.1", "--ipv6",
		"--subnet fd91:cafe:cafe:10::/64", "--gateway fd91:cafe:cafe:10::1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少参数 %q，实际：%s", want, joined)
		}
	}
}

// 目标形态为纯 v4 时不能带 --ipv6 及其段。
func TestCreateArgsV4Only(t *testing.T) {
	got := createArgs(Options{Network: "narwhal-net", IPv4: "10.91.0.0/20", Gateway4: "10.91.0.1"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--ipv6") {
		t.Fatalf("纯 v4 形态不应带 --ipv6：%s", joined)
	}
	if strings.Contains(joined, "--interface-name") {
		t.Fatalf("未指定桥名时不应传 --interface-name：%s", joined)
	}
}

// 形态相同时不重建，避免无谓的网络中断。
func TestSameShape(t *testing.T) {
	v4 := Options{IPv4: "10.91.0.0/20", IPv6: ""}
	v6 := Options{IPv4: "10.91.0.0/20", IPv6: "fd91:cafe:cafe:10::/64"}

	if !sameShape(State{Exists: true, IPv4: "10.91.0.0/20"}, v4) {
		t.Fatal("同为纯 v4 应判定一致")
	}
	if sameShape(State{Exists: true, IPv4: "10.91.0.0/20", IPv6: "fd91:cafe:cafe:10::/64"}, v4) {
		t.Fatal("现有是双栈、目标是纯 v4，应判定不一致")
	}
	if !sameShape(State{Exists: true, IPv4: "10.91.0.0/20", IPv6: "fd91:cafe:cafe:10::/64"}, v6) {
		t.Fatal("同为双栈应判定一致")
	}
	if sameShape(State{}, v6) {
		t.Fatal("网络不存在时不能判定一致")
	}
}
