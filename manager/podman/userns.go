package podman

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strings"
)

// usernsPoolEntry 是 userns=auto 默认使用的 subordinate ID 池。
// 与 install.sh 的 ensure_subuid 保持一致：起始 1000000、长度 6553600。
const usernsPoolEntry = "containers:1000000:6553600\n"

// hasSubidEntry 判断 /etc/subuid 或 /etc/subgid 里是否已有指定用户的行。
func hasSubidEntry(path, name string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), name+":") {
			return true
		}
	}
	return false
}

// EnsureUsernsPool 确保 userns=auto 所需的 subuid/subgid 池存在（幂等）。
//
// 为什么需要运行期兜底：libpod 的 userns=auto 从 **containers 用户**的
// /etc/subuid、/etc/subgid 池分配 ID，池缺失时创建容器会失败：
//
//	creating container storage: not enough unused IDs in user namespace
//
// 我们的 install.sh 有 ensure_subuid 会配好，但官方 install.sh 没有——
// 「官方安装 + 覆盖本 fork 二进制」这种部署方式会缺池，因此启动时补一次。
// 已存在 containers: 行时保留原值，不覆盖管理员自定义的池。
func EnsureUsernsPool() error {
	need := false
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if !hasSubidEntry(f, "containers") {
			need = true
			break
		}
	}
	if !need {
		return nil
	}

	// 用户不存在时先创建（仅作为 ID 池占位，不需要登录能力）。
	if _, err := user.Lookup("containers"); err != nil {
		out, uerr := exec.Command("useradd",
			"--comment", "Podman userns id pool",
			"--no-create-home",
			"--shell", "/usr/sbin/nologin",
			"containers").CombinedOutput()
		if uerr != nil {
			return fmt.Errorf("useradd containers: %v (%s)", uerr, strings.TrimSpace(string(out)))
		}
		log.Printf("[Podman] created helper user 'containers' for userns id pool")
	}

	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if hasSubidEntry(f, "containers") {
			continue
		}
		fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open %s: %w", f, err)
		}
		if _, err := fh.WriteString(usernsPoolEntry); err != nil {
			_ = fh.Close()
			return fmt.Errorf("append %s: %w", f, err)
		}
		if err := fh.Close(); err != nil {
			return fmt.Errorf("close %s: %w", f, err)
		}
		log.Printf("[Podman] configured userns id pool in %s", f)
	}
	return nil
}
