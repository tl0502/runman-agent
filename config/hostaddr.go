package config

import (
	"net"
	"sort"
	"strings"
)

// isVirtualInterface 判断网卡是否属于容器/隧道类虚拟接口，这些接口上的地址
// 不能作为对外监听地址（podman 的 podman1、docker0、veth 等）。
func isVirtualInterface(name string) bool {
	for _, prefix := range []string{"podman", "docker", "br-", "veth", "cni", "virbr", "vnet", "tun", "tap"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ListHostIPv4 枚举本机网卡上的 IPv4 地址（含内网地址——云厂商 1:1 NAT 机器
// 的监听地址就是内网网卡地址），过滤回环、链路本地与虚拟接口。
func ListHostIPv4() []string {
	result := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return result
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(ifi.Name) {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			result = append(result, ip4.String())
		}
	}
	sort.Strings(result)
	return result
}

// ListHostIPv6 枚举本机网卡上的全局 IPv6 地址。
// 过滤回环、链路本地与 ULA 私网（fc00::/7），只留可对外使用的公网地址。
func ListHostIPv6() []string {
	result := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return result
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(ifi.Name) {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() {
				continue
			}
			result = append(result, ip.String())
		}
	}
	sort.Strings(result)
	return result
}

// HostHasIPv4 判断地址是否已配置在本机网卡上。空值表示「不指定」，视为合法。
// 端口转发直接 bind 这个地址，地址不存在时 bind 会 EADDRNOTAVAIL；典型误配是
// 云厂商 1:1 NAT 机器（网卡只有内网地址）把弹性公网 IP 填进来——那个地址不能绑。
func HostHasIPv4(addr string) bool {
	if strings.TrimSpace(addr) == "" {
		return true
	}
	target := net.ParseIP(strings.TrimSpace(addr))
	if target == nil || target.To4() == nil {
		return false
	}
	for _, a := range ListHostIPv4() {
		if ip := net.ParseIP(a); ip != nil && ip.Equal(target) {
			return true
		}
	}
	return false
}

// HostHasIPv6 判断地址是否已配置在本机网卡的全局 IPv6 上。
func HostHasIPv6(addr string) bool {
	target := net.ParseIP(strings.TrimSpace(addr))
	if target == nil || target.To4() != nil {
		return false
	}
	for _, a := range ListHostIPv6() {
		if ip := net.ParseIP(a); ip != nil && ip.Equal(target) {
			return true
		}
	}
	return false
}
