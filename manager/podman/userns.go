package podman

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
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
	changed := false
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		ok, err := ensureSubidCapacity(f)
		if err != nil {
			return err
		}
		changed = changed || ok
	}
	if !changed {
		// 池容量够不代表守护进程用的是它：install.sh 可能先扩了池，而 podman 服务
		// 早就常驻并缓存了旧的 subid 范围。文件比服务启动新，就说明服务读到的是旧内容。
		if stale, detail := usernsPoolStaleForService(); stale {
			log.Printf("[Podman] %s, restarting podman.service to reload subuid", detail)
			if err := restartPodmanAPI(30 * time.Second); err != nil {
				log.Printf("[Podman] podman service refresh failed: %v", err)
			}
		}
		return nil
	}
	// 只写文件不够：真正分配 ID 的是常驻的 podman API 服务，storage 库在首次
	// userns=auto 之后就把可用 subid 范围缓存在内存里（store.additionalUIDs），
	// 此后不再重读 /etc/subuid。不重启守护进程的话，新扩的池对它不可见，
	// 升级后创建容器会一直报 not enough unused IDs，直到人工重启为止。
	if err := restartPodmanAPI(30 * time.Second); err != nil {
		log.Printf("[Podman] userns id pool extended but podman service refresh failed (new containers may still fail until it restarts): %v", err)
	}
	return nil
}

// usernsPoolStaleForService 判断 podman API 服务的启动时间是否早于 subuid/subgid
// 的修改时间（即有池变更发生在服务启动之后，服务内存里的池已过期）。
func usernsPoolStaleForService() (bool, string) {
	started, err := podmanServiceStartUnix()
	if err != nil {
		return false, ""
	}
	mtime, err := usernsPoolFileMtime()
	if err != nil {
		return false, ""
	}
	// 2 秒容差：安装脚本与 systemd 起服务的先后可能只差一点点。
	if mtime <= started+2 {
		return false, ""
	}
	return true, fmt.Sprintf("subuid pool changed at %s after podman.service started at %s",
		time.Unix(mtime, 0).Format(time.RFC3339), time.Unix(started, 0).Format(time.RFC3339))
}

// podmanServiceStartUnix 返回 podman.service 主进程的启动时间（unix 秒）。
func podmanServiceStartUnix() (int64, error) {
	out, err := exec.Command("systemctl", "show", "podman.service",
		"-p", "ExecMainStartTimestamp", "--timestamp=unix", "--value").Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimPrefix(strings.TrimSpace(string(out)), "@")
	if s == "" {
		return 0, fmt.Errorf("empty ExecMainStartTimestamp")
	}
	return strconv.ParseInt(s, 10, 64)
}

// usernsPoolFileMtime 返回 /etc/subuid 与 /etc/subgid 中较新的修改时间。
func usernsPoolFileMtime() (int64, error) {
	var newest int64
	var found bool
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if m := fi.ModTime().Unix(); m > newest {
			newest = m
		}
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no subuid/subgid file found")
	}
	return newest, nil
}

// usernsPoolExhaustedMsg 是 storage 库在可用 subid 不足时返回的错误文本。
const usernsPoolExhaustedMsg = "not enough unused IDs in user namespace"

// isUsernsPoolExhausted 判断创建失败是否属于「userns ID 池耗尽」。
// 这类错误几乎只有两种来源：池容量本来就不够，或扩池后常驻 podman 服务没重启
// （它缓存了旧的 subid 范围）。两者都可以靠重启服务 + 重试恢复。
func isUsernsPoolExhausted(err error) bool {
	return err != nil && strings.Contains(err.Error(), usernsPoolExhaustedMsg)
}

// restartPodmanAPI 重启 systemd 的 podman API 服务并等待 socket 恢复。
// 重启服务不会停止运行中的容器（容器由 conmon 托管），只中断 API 几秒。
func restartPodmanAPI(timeout time.Duration) error {
	out, err := exec.Command("systemctl", "is-active", "podman.service").CombinedOutput()
	state := strings.TrimSpace(string(out))
	if err != nil || state != "active" {
		return fmt.Errorf("podman.service is not active (%s)", state)
	}
	log.Printf("[Podman] restarting podman.service to reload subuid/subgid pool")
	if out, err := exec.Command("systemctl", "restart", "podman.service").CombinedOutput(); err != nil {
		return fmt.Errorf("restart podman.service: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cerr := bindings.NewConnection(context.Background(), SocketPath)
		if cerr == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, lastErr = bindings.GetClient(pingCtx)
			cancel()
			if lastErr == nil {
				log.Printf("[Podman] podman.service restarted, API is ready")
				return nil
			}
		} else {
			lastErr = cerr
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("podman API not ready after restart: %w", lastErr)
}

func ensureSubidCapacity(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if subidCapacity(string(data), "containers") >= uint64(usernsSize)*usernsMinContainers {
		return false, nil
	}

	// 补池前先确保占位用户存在（只有存在的用户才能写 subuid/subgid 行）。
	if _, err := user.Lookup("containers"); err != nil {
		out, uerr := exec.Command("useradd",
			"--comment", "Podman userns id pool",
			"--no-create-home",
			"--shell", "/usr/sbin/nologin",
			"containers").CombinedOutput()
		if uerr != nil {
			return false, fmt.Errorf("useradd containers: %v (%s)", uerr, strings.TrimSpace(string(out)))
		}
		log.Printf("[Podman] created helper user 'containers' for userns id pool")
	}

	// 默认大池已在但仍不满足容量：说明管理员主动改小了池，不重复追加
	// （同一个起始 ID 写两遍会让 libpod 分配到重叠区间）。
	if strings.Contains(string(data), usernsPoolEntry) {
		log.Printf("[Podman] %s: containers id pool is smaller than expected; new userns containers may fail", path)
		return false, nil
	}

	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := fh.WriteString(usernsPoolEntry + "\n"); err != nil {
		_ = fh.Close()
		return false, fmt.Errorf("append %s: %w", path, err)
	}
	if err := fh.Close(); err != nil {
		return false, fmt.Errorf("close %s: %w", path, err)
	}
	log.Printf("[Podman] extended userns id pool in %s (%s)", path, usernsPoolEntry)
	return true, nil
}
