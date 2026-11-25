package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runman-agent/config"
	"runman-agent/db"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	installScript = "/opt/narwhal-agent/install.sh"
)

type githubRelease struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	// Assets 携带每个资产的 sha256 摘要（GitHub 2025 起提供 digest 字段）。
	// 用于校验下载到的 install.sh 与二进制确实是该 release 的内容。
	Assets []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"` // 形如 "sha256:abcdef..."
}

type Service struct {
	database    *db.DB
	cfg         *config.Manager
	currentVer  string
	lastChecked time.Time
	updateMu    sync.Mutex
}

// releaseChannel 统一返回 latest。
//
// 发布采用语义化 tag（vX.Y.Z），GitHub 的 releases/latest 指向最新的正式发布；
// 历史上按 commit 滚动的 continuous 通道已废弃，但旧机器上跑的
// continuous-<sha> 仍需能查到发布版，因此不再按版本号分流，
// 由 VersionNewer 判定为“需要迁移到发布版”。
func releaseChannel(string) string {
	return "latest"
}

// releaseAPIURL 返回版本查询接口。配置了 update_api_url 时直接使用，
// 否则按 update_repo 派生 GitHub API 地址。
func (s *Service) releaseAPIURL() string {
	if base := s.cfg.Get().UpdateAPIBase(); base != "" {
		return strings.TrimRight(base, "/") + "/latest"
	}
	return "https://api.github.com/repos/" + s.cfg.Get().UpdateRepository() + "/releases/latest"
}

// releaseDownloadBase 返回 releases 下载基址。配置了 update_base_url 时直接使用，
// 否则按 update_repo 派生 GitHub 下载地址。
func (s *Service) releaseDownloadBase() string {
	if base := s.cfg.Get().UpdateDownloadBase(); base != "" {
		return base
	}
	// 语义化发布下，latest/download 恒指向最新正式版的资产。
	return "https://github.com/" + s.cfg.Get().UpdateRepository() + "/releases/latest/download"
}

// LatestVersion 返回远程最新发布版本号，供面板的“检查更新”使用。
// 地址与 updater 自身一致，避免面板查官方仓库、更新却走自有源的口径分裂。
func (s *Service) LatestVersion() (string, error) {
	return s.fetchLatestVersion()
}

// VersionNewer 判断 latest 是否比 current 更新。
//
// current 不是 v 前缀（历史上的 continuous-<sha> 构建）时视为需要迁移，
// 因为只要远端已有正式发布，它就应该换成发布版。
// 远端不是正式 tag 时一律返回 false，避免把异常数据当成“有新版本”。
//
// 本地补丁构建（v0.11.0-usernsfix / -dirty / -<sha> 这类自定义 ldflags）
// 不算「比同号正式版旧」：否则面板的「检查更新」会恒为 true，自动更新还会
// 把本地补丁覆盖掉。官方预发布（rc/beta/alpha）仍保留原语义。
func VersionNewer(latest, current string) bool {
	lMajor, lMinor, lPatch, lPre, lOK := parseSemver(latest)
	if !lOK {
		return false
	}
	cMajor, cMinor, cPatch, cPre, cOK := parseSemver(current)
	if !cOK {
		return true
	}

	switch {
	case lMajor != cMajor:
		return lMajor > cMajor
	case lMinor != cMinor:
		return lMinor > cMinor
	case lPatch != cPatch:
		return lPatch > cPatch
	}
	// 主版本号相同：本地补丁构建视为与该正式版等价，不提示更新、不触发覆盖。
	if cPre != "" && isLocalBuildPre(cPre) {
		return false
	}
	// 同号：正式版高于预发布，预发布之间按字典序（够用，不追求 SemVer 全序）。
	if lPre == cPre {
		return false
	}
	if lPre == "" {
		return true
	}
	if cPre == "" {
		return false
	}
	return lPre > cPre
}

// isLocalBuildPre 判断预发布后缀是不是本地自建标记。
//
// 官方预发布只认 rc/beta/alpha/preview（后接数字或点分层级，如 rc1、beta.2）；
// 其余（usernsfix、dirty、git sha、任意自定义词）都按「基于该正式版改出来的
// 本地构建」处理。build metadata（+xxx）在 parseSemver 里已被剥离，天然等价。
func isLocalBuildPre(pre string) bool {
	p := strings.ToLower(pre)
	for _, tag := range []string{"rc", "beta", "alpha", "preview"} {
		if !strings.HasPrefix(p, tag) {
			continue
		}
		rest := p[len(tag):]
		if rest == "" || rest[0] == '.' || (rest[0] >= '0' && rest[0] <= '9') {
			return false
		}
	}
	return true
}

// parseSemver 解析 vMAJOR.MINOR.PATCH[-pre][+build]。不允许缺段。
func parseSemver(v string) (major, minor, patch int, pre string, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		return 0, 0, 0, "", false
	}
	core := v[1:]
	if i := strings.IndexByte(core, '+'); i >= 0 {
		core = core[:i]
	}
	if i := strings.IndexByte(core, '-'); i >= 0 {
		pre = core[i+1:]
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, 0, "", false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, "", false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], pre, true
}

func releaseVersion(rel githubRelease, channel string) (string, error) {
	// releaseChannel 恒返回 "latest"，continuous 通道已废弃（见 releaseChannel 注释），
	// 因此这里只处理语义化 tag。
	_ = channel
	if !strings.HasPrefix(rel.TagName, "v") {
		return "", fmt.Errorf("invalid stable release tag %q", rel.TagName)
	}
	return rel.TagName, nil
}

func NewService(database *db.DB, cfg *config.Manager, currentVer string) *Service {
	return &Service{
		database:   database,
		cfg:        cfg,
		currentVer: currentVer,
	}
}

func (s *Service) Start(ctx context.Context) {
	log.Printf("[Updater] Service started (current: %s)", s.currentVer)
	if os.Getenv("RUNMAN_AGENT_AUTO_UPDATE") == "0" {
		log.Printf("[Updater] Automatic updates disabled by RUNMAN_AGENT_AUTO_UPDATE=0")
		return
	}

	// 如果是开发版本，不执行自动更新
	if s.currentVer == "dev" || s.currentVer == "" {
		log.Printf("[Updater] Dev version detected, auto-update disabled")
		return
	}

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	// 启动时先检查一次
	s.checkAndRun(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAndRun(ctx)
		}
	}
}

// ManualUpdate 供 Web 端或其他地方手动触发立即更新
func (s *Service) ManualUpdate() error {
	log.Printf("[Updater] Manual update requested")
	return s.executeUpdate()
}

func (s *Service) checkAndRun(ctx context.Context) {
	latest, err := s.fetchLatestVersion()
	if err != nil {
		log.Printf("[Updater] Fetch latest version failed: %v", err)
		return
	}

	// 没有更新的版本（含相等与回滚场景），清理数据库中的待更新记录
	if !VersionNewer(latest, s.currentVer) {
		// log.Printf("[Updater] Current version %s is up-to-date", s.currentVer)
		_ = s.database.SetSystem("pending_update_ver", "")
		_ = s.database.SetSystem("update_target_time", "")
		return
	}

	log.Printf("[Updater] New version detected: %s (current: %s)", latest, s.currentVer)

	// 检查数据库中是否已有该版本的待更新记录
	pendingVer, _ := s.database.GetSystem("pending_update_ver")
	if pendingVer != latest {
		// 记录新版本发现时间，并随机生成一个 24-72 小时后的执行时间
		log.Printf("[Updater] Recording new update path for %s", latest)
		_ = s.database.SetSystem("pending_update_ver", latest)

		// 随机 24-72 小时 (86400 - 259200 秒)
		randomDelay := 86400 + rand.Intn(172800)
		targetTime := time.Now().Add(time.Duration(randomDelay) * time.Second)
		_ = s.database.SetSystem("update_target_time", targetTime.Format(time.RFC3339))

		log.Printf("[Updater] Scheduled update to %s at %s", latest, targetTime.Format("2006-01-02 15:04:05"))
		return
	}

	// 检查是否到达执行时间
	targetTimeStr, _ := s.database.GetSystem("update_target_time")
	if targetTimeStr == "" {
		return
	}

	targetTime, err := time.Parse(time.RFC3339, targetTimeStr)
	if err != nil {
		return
	}

	if time.Now().After(targetTime) {
		log.Printf("[Updater] REACHED target time! Executing forced update to %s...", latest)
		if err := s.executeUpdate(); err != nil {
			log.Printf("[Updater] Update failed; Agent remains running: %v", err)
		}
	} else {
		log.Printf("[Updater] Waiting for scheduled update at %s (remains: %v)",
			targetTime.Format("2006-01-02 15:04:05"),
			time.Until(targetTime).Round(time.Minute))
	}
}

func (s *Service) fetchLatestVersion() (string, error) {
	rel, err := s.fetchRelease()
	if err != nil {
		return "", err
	}
	return releaseVersion(rel, releaseChannel(s.currentVer))
}

// fetchRelease 拉取 release 元数据（含资产 digest）。
func (s *Service) fetchRelease() (githubRelease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", s.releaseAPIURL(), nil)
	if err != nil {
		return githubRelease{}, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return githubRelease{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("github api returned %d", resp.StatusCode)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return githubRelease{}, err
	}
	return rel, nil
}

// assetDigest 返回指定资产名在该 release 中的 sha256 摘要。
// 找不到资产或该 release 未提供 digest 时返回空串（调用方决定是否放行）。
func (r githubRelease) assetDigest(name string) string {
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		if strings.HasPrefix(a.Digest, "sha256:") {
			return strings.TrimPrefix(a.Digest, "sha256:")
		}
	}
	return ""
}

func (s *Service) executeUpdate() error {
	if !s.updateMu.TryLock() {
		return fmt.Errorf("update already being prepared")
	}
	defer s.updateMu.Unlock()
	// 1. 先下载最新的安装脚本
	if err := s.downloadInstallScript(); err != nil {
		log.Printf("[Updater] Failed to download latest install script: %v", err)
		// Never fall back to a cached script if download fails.
		return err
	}
	if output, err := exec.Command("bash", "-n", installScript).CombinedOutput(); err != nil {
		return fmt.Errorf("installer syntax check: %w: %s", err, output)
	}

	log.Printf("[Updater] Running %s...", installScript)

	// A separate systemd unit survives stopping/restarting the Agent, including
	// upstream installations with KillMode=control-group. A fixed unit name also
	// rejects concurrent manual/automatic update jobs.
	cmd := exec.Command("systemd-run", s.updateCommandArgs()...)

	// 我们不等待脚本完成，因为脚本会重启服务导致我们被 kill
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[Updater] Failed to start update script: %v", err)
		return fmt.Errorf("start update unit: %w: %s", err, output)
	}

	log.Printf("[Updater] Update unit started; inspect journalctl -u narwhal-agent-update")

	return nil
}

func (s *Service) updateCommandArgs() []string {
	return []string{"--unit=narwhal-agent-update", "--collect", "--property=Type=exec",
		"--property=WorkingDirectory=/", "--setenv=AGENT_RELEASE_TAG=" + releaseChannel(s.currentVer),
		"--setenv=RUNMAN_AGENT_DOWNLOAD_BASE=" + s.releaseDownloadBase(),
		"--setenv=NETAVARK_BASE=" + s.cfg.Get().NetavarkDownloadBase(),
		"bash", installScript, "en", "--update-only", "--non-interactive"}
}

// downloadInstallScript 下载安装脚本并校验其 sha256。
//
// 安全要点：下载到的 install.sh 会以 root 身份被执行（systemd-run），
// 因此必须确认内容确实是目标 release 的那份，不能只做语法检查。
// 期望摘要取自 GitHub release API 的资产 digest 字段。
// 自建镜像源若不提供 digest，则退化为「仅 https + 语法检查」——
// 此时由配置层的域名/协议校验（config.ValidateDownloadBase）兜底。
func (s *Service) downloadInstallScript() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	expected := ""
	if rel, err := s.fetchRelease(); err == nil {
		expected = rel.assetDigest("install.sh")
	} else {
		log.Printf("[Updater] Could not fetch release digest for install.sh: %v", err)
	}

	return downloadScript(ctx, http.DefaultClient, s.releaseDownloadBase()+"/install.sh", installScript, expected)
}

// downloadScript 下载脚本到 destination。
//
// expectedSHA256 非空时校验内容摘要——不匹配则拒绝落盘。这是防止下载源被
// 劫持后注入恶意脚本的关键一环（脚本会以 root 身份执行）。
// expectedSHA256 为空表示上游未提供摘要（自建镜像源），此时仅做大小检查。
func downloadScript(ctx context.Context, client *http.Client, scriptURL, destination, expectedSHA256 string) error {

	req, err := http.NewRequestWithContext(ctx, "GET", scriptURL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download script failed: status %d", resp.StatusCode)
	}

	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(destination), ".installer-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	const maxScriptSize = 2 << 20
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(resp.Body, maxScriptSize+1))
	if err != nil {
		return err
	}
	if n == 0 || n > maxScriptSize {
		return fmt.Errorf("invalid installer size: %d", n)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if expectedSHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, expectedSHA256) {
			return fmt.Errorf("installer sha256 mismatch: got %s, want %s", got, expectedSHA256)
		}
	}
	return os.Rename(f.Name(), destination)
}
