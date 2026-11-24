// Package netmigrate 重建 podman 网络，用于在「仅 IPv4」与「双栈」之间切换。
//
// podman 的 IPv6 是网络级属性：容器有没有 v6 地址由所在网络决定，容器级没有
// 开关。所以给一台已经装好的 IPv4-only 机器加共享 IPv6，必须重建网络。
//
// 重建方式：先让容器逐个脱离网络（运行中的容器也可以 disconnect，进程不重启），
// 再删除网络、按目标形态重建，最后用原来的静态地址接回去。代价是网络中断数秒、
// 已建立的连接会断开，但容器内的进程、状态、磁盘都不受影响。
package netmigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// Options 描述目标网络形态。
type Options struct {
	Network   string // 网络名，通常 narwhal-net
	Interface string // 桥接口名，如 podman1；空则由 podman 分配
	IPv4      string // v4 子网，如 10.91.0.0/20
	Gateway4  string // v4 网关
	IPv6      string // v6 子网；空表示不要 v6（双栈拉平为纯 v4 网络）
	Gateway6  string // v6 网关
}

// Result 汇报一次迁移的结果。
type Result struct {
	Containers int           // 接回的容器数
	Downtime   time.Duration // 网络中断时长
	Rebuilt    bool          // 是否真的重建了网络（形态未变时跳过）
}

// Container 记录容器的网络绑定，用于重建后恢复。
type Container struct {
	Name string
	IPv4 string
	IPv6 string
	MAC  string
}

// State 是网络的当前形态。
type State struct {
	Exists    bool
	IPv4      string
	Gateway4  string
	IPv6      string
	Gateway6  string
	Interface string
}

// Rebuild 把网络改造成 want 描述的形态，并接回所有使用它的容器。
// 形态已经匹配时直接返回（Rebuilt=false）。
// 中途失败会尽力把网络恢复成改造前的样子，避免容器全部失去网络。
func Rebuild(ctx context.Context, want Options) (*Result, error) {
	start := time.Now()

	cur, err := Inspect(ctx, want.Network)
	if err != nil {
		return nil, err
	}
	if cur.Exists && sameShape(cur, want) {
		return &Result{Rebuilt: false}, nil
	}

	containers, err := listContainers(ctx, want.Network)
	if err != nil {
		return nil, err
	}

	// 原网络不存在时没东西可删，直接用目标形态建。
	if cur.Exists {
		if cur.Interface != "" && want.Interface == "" {
			want.Interface = cur.Interface
		}
		if err := disconnectAll(ctx, want.Network, containers); err != nil {
			return nil, err
		}
		if out, err := run(ctx, "podman", "network", "rm", want.Network); err != nil {
			if reattachAll(ctx, want.Network, containers) != nil {
				log.Printf("[NetMigrate] 回滚接回失败，容器可能需要手工恢复网络")
			}
			return nil, fmt.Errorf("删除网络失败：%v（%s）", err, strings.TrimSpace(out))
		}
	}

	if out, err := run(ctx, "podman", append([]string{"network", "create"}, createArgs(want)...)...); err != nil {
		// 新网络建不起来：把原形态恢复回去，别让容器悬空。
		rollback := Options{
			Network: want.Network, Interface: cur.Interface,
			IPv4: cur.IPv4, Gateway4: cur.Gateway4,
			IPv6: cur.IPv6, Gateway6: cur.Gateway6,
		}
		if cur.Exists {
			if _, rerr := run(ctx, "podman", append([]string{"network", "create"}, createArgs(rollback)...)...); rerr == nil {
				if reattachAll(ctx, want.Network, containers) != nil {
					log.Printf("[NetMigrate] 回滚接回失败，容器可能需要手工恢复网络")
				}
			}
		}
		return nil, fmt.Errorf("创建网络失败：%v（%s）", err, strings.TrimSpace(out))
	}

	if err := reattachAll(ctx, want.Network, containers); err != nil {
		return nil, err
	}

	res := &Result{Containers: len(containers), Downtime: time.Since(start), Rebuilt: true}
	log.Printf("[NetMigrate] 网络已重建：v4=%s v6=%s，接回 %d 个容器，中断 %s",
		want.IPv4, describeNet(want.IPv6), res.Containers, res.Downtime.Round(time.Second))
	return res, nil
}

func describeNet(s string) string {
	if s == "" {
		return "<无>"
	}
	return s
}

// createArgs 返回 podman network create 的参数（不含子命令本身）。
func createArgs(o Options) []string {
	args := []string{"--driver=bridge", "--disable-dns"}
	if o.Interface != "" {
		args = append(args, "--interface-name", o.Interface)
	}
	if o.IPv4 != "" {
		args = append(args, "--subnet", o.IPv4)
		if o.Gateway4 != "" {
			args = append(args, "--gateway", o.Gateway4)
		}
	}
	if o.IPv6 != "" {
		args = append(args, "--ipv6", "--subnet", o.IPv6)
		if o.Gateway6 != "" {
			args = append(args, "--gateway", o.Gateway6)
		}
	}
	return append(args, o.Network)
}

// sameShape 判断现有网络是否已经是目标形态：v4 段与 v6 段一致即视为一致。
func sameShape(cur State, want Options) bool {
	return cur.Exists && cur.IPv4 == want.IPv4 && cur.IPv6 == want.IPv6
}

// Inspect 读取网络当前形态；网络不存在时返回零值（Exists=false）。
func Inspect(ctx context.Context, name string) (State, error) {
	out, err := run(ctx, "podman", "network", "inspect", name)
	if err != nil {
		// 网络不存在不是错误：调用方会直接建。
		if strings.Contains(strings.ToLower(out), "not found") ||
			strings.Contains(strings.ToLower(err.Error()), "not found") {
			return State{}, nil
		}
		return State{}, fmt.Errorf("读取网络 %s 失败：%v（%s）", name, err, strings.TrimSpace(out))
	}

	var parsed []struct {
		Subnets []struct {
			Subnet  string `json:"subnet"`
			Gateway string `json:"gateway"`
		} `json:"subnets"`
		Interface string `json:"network_interface"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return State{}, fmt.Errorf("解析网络信息失败：%w", err)
	}
	st := State{Exists: true}
	if len(parsed) == 0 {
		return st, nil
	}
	st.Interface = parsed[0].Interface
	for _, s := range parsed[0].Subnets {
		if strings.Contains(s.Subnet, ":") {
			st.IPv6, st.Gateway6 = s.Subnet, s.Gateway
		} else {
			st.IPv4, st.Gateway4 = s.Subnet, s.Gateway
		}
	}
	return st, nil
}

func listContainers(ctx context.Context, network string) ([]Container, error) {
	out, err := run(ctx, "podman", "ps", "-a", "--filter", "network="+network, "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("列出容器失败：%v（%s）", err, strings.TrimSpace(out))
	}
	var result []Container
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		c, err := inspectContainerNet(ctx, name, network)
		if err != nil {
			return nil, err
		}
		if c == nil {
			continue // 已经不在这个网络上
		}
		result = append(result, *c)
	}
	return result, nil
}

// inspectContainerNet 读取容器在网络里的静态地址；容器不在该网络时返回 nil。
func inspectContainerNet(ctx context.Context, name, network string) (*Container, error) {
	out, err := run(ctx, "podman", "inspect", name, "--format", "{{json .NetworkSettings.Networks}}")
	if err != nil {
		return nil, fmt.Errorf("读取容器 %s 网络信息失败：%v（%s）", name, err, strings.TrimSpace(out))
	}
	var nets map[string]struct {
		IPAddress         string `json:"IPAddress"`
		GlobalIPv6Address string `json:"GlobalIPv6Address"`
		MacAddress        string `json:"MacAddress"`
	}
	if err := json.Unmarshal([]byte(out), &nets); err != nil {
		return nil, fmt.Errorf("解析容器 %s 网络信息失败：%w", name, err)
	}
	n, ok := nets[network]
	if !ok {
		return nil, nil
	}
	return &Container{Name: name, IPv4: n.IPAddress, IPv6: n.GlobalIPv6Address, MAC: n.MacAddress}, nil
}

// disconnectAll 让容器逐个脱离网络；中途失败时把已经断开的接回去，
// 避免半迁移状态（部分容器没网、部分还在旧网络上）。
func disconnectAll(ctx context.Context, network string, cs []Container) error {
	done := make([]Container, 0, len(cs))
	for _, c := range cs {
		if out, err := run(ctx, "podman", "network", "disconnect", network, c.Name); err != nil {
			if rerr := reattachAll(ctx, network, done); rerr != nil {
				log.Printf("[NetMigrate] 回滚接回失败：%v", rerr)
			}
			return fmt.Errorf("容器 %s 脱离网络失败：%v（%s）", c.Name, err, strings.TrimSpace(out))
		}
		done = append(done, c)
	}
	return nil
}

func reattachAll(ctx context.Context, network string, cs []Container) error {
	attached := 0
	for _, c := range cs {
		args := []string{"network", "connect"}
		if c.IPv4 != "" {
			args = append(args, "--ip", c.IPv4)
		}
		if c.IPv6 != "" {
			args = append(args, "--ip6", c.IPv6)
		}
		if c.MAC != "" {
			args = append(args, "--mac-address", c.MAC)
		}
		args = append(args, network, c.Name)
		if out, err := run(ctx, "podman", args...); err != nil {
			return fmt.Errorf("容器 %s 接回网络失败：%v（%s），已接回 %d/%d",
				c.Name, err, strings.TrimSpace(out), attached, len(cs))
		}
		attached++
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
