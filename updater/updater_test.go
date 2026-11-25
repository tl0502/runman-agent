package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"runman-agent/config"
)

// 发布统一走语义化 tag，任何本地版本都必须查 releases/latest。
func TestReleaseChannelIsAlwaysLatest(t *testing.T) {
	for _, version := range []string{"main", "continuous", "continuous-" + strings.Repeat("a", 40), "v1.2.3", "dev-abcdef0"} {
		t.Run(version, func(t *testing.T) {
			h := newTestService(t, version, "{}")
			api, base := h.releaseAPIURL(), h.releaseDownloadBase()
			if !strings.Contains(api, "repos/tl0502/runman-agent/") || !strings.Contains(base, "/tl0502/runman-agent/") {
				t.Fatalf("wrong repository: %s %s", api, base)
			}
			if !strings.HasSuffix(api, "/releases/latest") || !strings.HasSuffix(base, "/releases/latest/download") {
				t.Fatalf("expected latest channel, got %s %s", api, base)
			}
			args := strings.Join(h.updateCommandArgs(), " ")
			for _, required := range []string{"--unit=narwhal-agent-update", "--update-only", "--non-interactive", "--setenv=RUNMAN_AGENT_DOWNLOAD_BASE=" + base} {
				if !strings.Contains(args, required) {
					t.Fatalf("missing %s: %s", required, args)
				}
			}
			if !strings.Contains(args, "tl0502/runman-agent") {
				t.Fatal("expected tl0502 update source")
			}
			if !strings.Contains(args, "--setenv=AGENT_RELEASE_TAG=latest") {
				t.Fatalf("installer must resolve latest: %s", args)
			}
		})
	}
}

// newTestService 构造一个使用空配置（即全部走内置默认）的 Service 用于测试地址派生。
func newTestService(t *testing.T, version, body string) *Service {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return &Service{cfg: cfg, currentVer: version}
}

func TestStableVersionValidation(t *testing.T) {
	if _, err := releaseVersion(githubRelease{}, "latest"); err == nil {
		t.Fatal("accepted empty stable version")
	}
	if _, err := releaseVersion(githubRelease{TagName: "continuous"}, "latest"); err == nil {
		t.Fatal("accepted non-v tag as stable")
	}
	if got, err := releaseVersion(githubRelease{TagName: "v0.5.3"}, "latest"); err != nil || got != "v0.5.3" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDownloadScriptIsAtomic(t *testing.T) {
	for _, scenario := range []string{"ok", "http-error", "empty", "too-large", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "install.sh")
			old := "old upstream script - must never run on error"
			if err := os.WriteFile(dest, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "ok":
					_, _ = w.Write([]byte("#!/bin/bash\necho updated\n"))
				case "http-error":
					w.WriteHeader(503)
				case "empty":
					w.WriteHeader(200)
				case "too-large":
					_, _ = w.Write([]byte(strings.Repeat("x", (2<<20)+1)))
				case "truncated":
					w.Header().Set("Content-Length", "1000")
					_, _ = w.Write([]byte("partial"))
				}
			}))
			defer server.Close()
			err := downloadScript(context.Background(), server.Client(), server.URL, dest, "")
			data, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "ok" {
				if err != nil || !strings.Contains(string(data), "echo updated") {
					t.Fatalf("%v %s", err, data)
				}
			} else if err == nil || string(data) != old {
				t.Fatalf("failed download replaced original: %v %q", err, data)
			}
			files, _ := filepath.Glob(filepath.Join(dir, ".installer-*"))
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}

// TestDownloadScriptVerifiesSHA256 验证摘要不匹配时拒绝落盘。
//
// 这是防止下载源被劫持后注入恶意脚本的关键一环：install.sh 会以 root 身份
// 被 systemd-run 执行，所以内容必须与目标 release 的资产摘要一致。
func TestDownloadScriptVerifiesSHA256(t *testing.T) {
	body := "#!/bin/bash\necho updated\n"
	sum := sha256.Sum256([]byte(body))

	for _, tc := range []struct {
		name     string
		expected string
		wantErr  bool
	}{
		{"match", hex.EncodeToString(sum[:]), false},
		{"mismatch", strings.Repeat("0", 64), true},
		{"empty-expected-skips-check", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "install.sh")
			old := "old upstream script"
			if err := os.WriteFile(dest, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			err := downloadScript(context.Background(), server.Client(), server.URL, dest, tc.expected)
			data, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected sha256 mismatch error, got nil")
				}
				if string(data) != old {
					t.Fatalf("mismatched script replaced original: %q", data)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(data) != body {
				t.Fatalf("unexpected content: %q", data)
			}
		})
	}
}

// TestAssetDigestLookup 验证从 release 元数据中提取资产摘要。
func TestAssetDigestLookup(t *testing.T) {
	rel := githubRelease{
		TagName: "v0.5.3",
		Assets: []githubAsset{
			{Name: "install.sh", Digest: "sha256:01e34f9446da2e61ec61f7b6558a9070f3c90cffc2b6544408d583f5c470afed"},
			{Name: "rfw-amd64", Digest: "sha256:6df2fb50e6aa1457b6c20f5af1c69663e6afb1b9658f10baac497193a1186d22"},
			{Name: "no-digest"},
		},
	}
	if got := rel.assetDigest("install.sh"); got != "01e34f9446da2e61ec61f7b6558a9070f3c90cffc2b6544408d583f5c470afed" {
		t.Fatalf("unexpected digest: %q", got)
	}
	if got := rel.assetDigest("missing"); got != "" {
		t.Fatalf("expected empty digest for missing asset, got %q", got)
	}
	if got := rel.assetDigest("no-digest"); got != "" {
		t.Fatalf("expected empty digest for asset without digest, got %q", got)
	}
}

// TestEndpointOverrides 验证接入点配置能覆盖内置默认，且空值回落到默认。
func TestEndpointOverrides(t *testing.T) {
	body := `{"platform_addr":"agent.example.com:8443","update_repo":"tl0502/runman-agent",` +
		`"update_base_url":"https://mirror.example.com/rel/continuous/",` +
		`"update_api_url":"https://mirror.example.com/api/releases"}`
	s := newTestService(t, "continuous", body)

	if got, want := s.cfg.Get().PlatformAddress(), "agent.example.com:8443"; got != want {
		t.Fatalf("platform addr = %q, want %q", got, want)
	}
	if got, want := s.releaseDownloadBase(), "https://mirror.example.com/rel/continuous"; got != want {
		t.Fatalf("download base = %q, want %q", got, want)
	}
	if got, want := s.releaseAPIURL(), "https://mirror.example.com/api/releases/latest"; got != want {
		t.Fatalf("api url = %q, want %q", got, want)
	}

	// 仅改仓库、不改基址时，应从新仓库派生 GitHub 地址。
	repoOnly := newTestService(t, "v1.2.3", `{"update_repo":"https://github.com/acme/agent/"}`)
	if got, want := repoOnly.releaseDownloadBase(), "https://github.com/acme/agent/releases/latest/download"; got != want {
		t.Fatalf("repo-only download base = %q, want %q", got, want)
	}
	if got, want := repoOnly.cfg.Get().PlatformAddress(), config.DefaultPlatformAddr; got != want {
		t.Fatalf("default platform addr = %q, want %q", got, want)
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
		why             string
	}{
		{"v0.4.1", "v0.4.0", true, "patch 递增"},
		{"v0.5.0", "v0.4.9", true, "minor 递增"},
		{"v1.0.0", "v0.9.9", true, "major 递增"},
		{"v0.4.0", "v0.4.0", false, "同版本"},
		{"v0.4.0", "v0.5.0", false, "远端更旧，不应提示更新"},
		{"v0.4.0", "continuous-" + strings.Repeat("a", 40), true, "旧 continuous 构建需迁移到发布版"},
		{"v0.4.0", "dev-abcdef0", true, "dev 构建可被发布版覆盖"},
		{"v0.4.0", "v0.4.0-rc1", true, "正式版高于预发布"},
		{"v0.4.0-rc2", "v0.4.0-rc1", true, "预发布之间按字典序"},
		{"v0.4.0-rc1", "v0.4.0", false, "预发布不高于正式版"},
		{"v0.4.0", "v0.4.0-usernsfix", false, "本地补丁构建不提示更新（Q17）"},
		{"v0.4.0", "v0.4.0-dirty", false, "dirty 构建不提示更新"},
		{"v0.4.0", "v0.4.0-deadbeef", false, "sha 后缀构建不提示更新"},
		{"v0.4.1", "v0.4.0-usernsfix", true, "补丁构建遇到更高正式版仍要更新"},
		{"v0.4.0", "v0.4.0+build7", false, "build metadata 等价于正式版"},
		{"continuous", "v0.4.0", false, "远端不是正式 tag"},
		{"main", "v0.4.0", false, "远端不是正式 tag"},
		{"", "v0.4.0", false, "空值"},
		{"v0.4", "v0.3.9", false, "段数不足视为非法"},
	}
	for _, c := range cases {
		if got := VersionNewer(c.latest, c.current); got != c.want {
			t.Errorf("VersionNewer(%q, %q) = %v, want %v (%s)", c.latest, c.current, got, c.want, c.why)
		}
	}
}
