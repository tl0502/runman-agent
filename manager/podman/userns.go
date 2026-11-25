package podman

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

const (
	// usernsPoolEntry 是 userns=auto 默认使用的 subordinate ID 池。
	// 与 install.sh 的 ensure_subuid 保持一致：起始 1000000、长度 6553600。
	usernsPoolEntry = "containers:1000000:6553600"

	// usernsSize 是单个容器分配的用户命名空间 ID 数量。
	//
	// 必须大于 65534：映射从 0 开始连续分配，userns=auto 的默认启发式只给
	// 1024 个 ID，容器内 GID 65534(nogroup) 因此不存在。Debian 系镜像的
	// sshd 以 uid=995 gid=65534 运行，preauth 阶段 setgroups() 返回 EINVAL，
	// sshd-session 直接 fatal，SSH 连接被关闭（表现为 Connection closed）。
	// 65536 可覆盖 0..65535（含 nogroup），也是常用的标准映射大小。
	usernsSize = 65536

	// usernsMinContainers 是 subuid/subgid 池至少要能支撑的容器数量。
	// 池容量若正好等于单容器需求（如 165536:65536），已有容器占用后再创建
	// 新容器会报 "not enough unused IDs in user namespace"，因此要求留有余量。
	usernsMinContainers = 8
)

// subidCapacity 统计文件中某个用户被分配到的 ID 总数。
// subuid 允许同一用户多条区间，这里累加；忽略空行与注释。
func subidCapacity(data, name string) uint64 {
	var total uint64
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name+":") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			continue
		}
		n, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil {
			continue
		}
		total += n
	}
	return total
}

// EnsureUsernsPool 确保 userns=auto 所需的 subuid/subgid 池存在且容量足够（幂等）。
//
// 为什么需要运行期兜底：libpod 的 userns=auto 从 **containers 用户**的
// /etc/subuid、/etc/subgid 池分配 ID，池缺失时创建容器会失败：
//
//	creating container storage: not enough unused IDs in user namespace
//
// 我们的 install.sh 有 ensure_subuid 会配好，但官方 install.sh 没有——
// 「官方安装 + 覆盖本 fork 二进制」这种部署方式会缺池，因此启动时补一次。
// 老安装器写入的池可能只有 65536（正好等于单个容器需求，没有余量），
// 这里按 usernsMinContainers 校验容量，不足时追加默认大池。
// 已存在的区间不会被改写或删除（改写会打乱存量容器的 uid 映射）。
func EnsureUsernsPool() error {
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if err := ensureSubidCapacity(f); err != nil {
			return err
		}
	}
	return nil
}

func ensureSubidCapacity(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if subidCapacity(string(data), "containers") >= uint64(usernsSize)*usernsMinContainers {
		return nil
	}

	// 补池前先确保占位用户存在（只有存在的用户才能写 subuid/subgid 行）。
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

	// 默认大池已在但仍不满足容量：说明管理员主动改小了池，不重复追加
	// （同一个起始 ID 写两遍会让 libpod 分配到重叠区间）。
	if strings.Contains(string(data), usernsPoolEntry) {
		log.Printf("[Podman] %s: containers id pool is smaller than expected; new userns containers may fail", path)
		return nil
	}

	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := fh.WriteString(usernsPoolEntry + "\n"); err != nil {
		_ = fh.Close()
		return fmt.Errorf("append %s: %w", path, err)
	}
	if err := fh.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	log.Printf("[Podman] extended userns id pool in %s (%s)", path, usernsPoolEntry)
	return nil
}
