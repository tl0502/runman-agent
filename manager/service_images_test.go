package manager

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"runman-agent/db"
	"runman-agent/proto/agent"
)

func newImageTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Init(filepath.Join(t.TempDir(), "images.db"))
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func builtinImages() []*agent.OSImageInfo {
	return []*agent.OSImageInfo{
		{Id: "docker.io/narwhalcloud/debian:podman", Name: "Debian (Podman)"},
		{Id: "docker.io/narwhalcloud/alpine:podman", Name: "Alpine (Podman)"},
	}
}

func idsOf(images []*agent.OSImageInfo) []string {
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = img.Id
	}
	return out
}

func TestBuildImageListDefaultsAllEnabled(t *testing.T) {
	d := newImageTestDB(t)
	got := idsOf(BuildImageList(d, builtinImages()))
	want := []string{"docker.io/narwhalcloud/debian:podman", "docker.io/narwhalcloud/alpine:podman"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestLegacyConfigMigration(t *testing.T) {
	d := newImageTestDB(t)
	// 旧格式：类别名逗号分隔，且顺序为 alpine 在前
	if err := d.SetSystem(osImagesConfigKey, "alpine,debian"); err != nil {
		t.Fatal(err)
	}
	got := idsOf(BuildImageList(d, builtinImages()))
	want := []string{"docker.io/narwhalcloud/alpine:podman", "docker.io/narwhalcloud/debian:podman"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("want %v, got %v", want, got)
	}
	// 迁移后应回写为 JSON
	raw, _ := d.GetSystem(osImagesConfigKey)
	if raw == "" || raw[0] != '[' {
		t.Fatalf("legacy config not migrated, raw=%q", raw)
	}
	var settings []imageSetting
	if err := json.Unmarshal([]byte(raw), &settings); err != nil || len(settings) != 2 {
		t.Fatalf("migrated settings invalid: %v %q", err, raw)
	}
}

func TestDisabledImageExcluded(t *testing.T) {
	d := newImageTestDB(t)
	_ = SaveImageConfigs(d, []ImageConfigItem{
		{ID: "docker.io/narwhalcloud/debian:podman", Enabled: true},
		{ID: "docker.io/narwhalcloud/alpine:podman", Enabled: false},
	})
	got := idsOf(BuildImageList(d, builtinImages()))
	if len(got) != 1 || got[0] != "docker.io/narwhalcloud/debian:podman" {
		t.Fatalf("disabled image should be excluded, got %v", got)
	}
	// 下架的镜像仍应出现在管理清单中（可再次上架）
	items := ListImageConfigs(d, builtinImages())
	if len(items) != 2 || items[1].Enabled {
		t.Fatalf("disabled image missing from config list: %+v", items)
	}
}

func TestCustomImageLifecycle(t *testing.T) {
	d := newImageTestDB(t)
	_ = d.UpsertCustomImage(db.CustomImage{ID: "docker.io/u/custom:1", Name: "Custom", Status: db.CustomImagePulling})

	// pulling 状态：管理清单可见（带状态），对租户列表不可见
	items := ListImageConfigs(d, builtinImages())
	found := false
	for _, it := range items {
		if it.ID == "docker.io/u/custom:1" {
			found = true
			if it.Status != db.CustomImagePulling || it.Kind != "custom" || !it.Enabled {
				t.Fatalf("unexpected custom item: %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("pulling custom image missing from config list: %+v", items)
	}
	if got := idsOf(BuildImageList(d, builtinImages())); len(got) != 2 {
		t.Fatalf("pulling image must not be visible, got %v", got)
	}

	// ready 后默认上架并追加末尾
	_ = d.UpdateCustomImageStatus("docker.io/u/custom:1", db.CustomImageReady, "")
	got := idsOf(BuildImageList(d, builtinImages()))
	if len(got) != 3 || got[2] != "docker.io/u/custom:1" {
		t.Fatalf("ready custom image should be appended, got %v", got)
	}

	// 下架后不可见
	_ = SaveImageConfigs(d, []ImageConfigItem{
		{ID: "docker.io/narwhalcloud/debian:podman", Enabled: true},
		{ID: "docker.io/narwhalcloud/alpine:podman", Enabled: true},
		{ID: "docker.io/u/custom:1", Enabled: false},
	})
	if got := idsOf(BuildImageList(d, builtinImages())); len(got) != 2 {
		t.Fatalf("disabled custom image must not be visible, got %v", got)
	}

	// 删除自定义镜像时清理配置
	if err := RemoveImageSetting(d, builtinImages(), "docker.io/u/custom:1"); err != nil {
		t.Fatal(err)
	}
	raw, _ := d.GetSystem(osImagesConfigKey)
	var settings []imageSetting
	_ = json.Unmarshal([]byte(raw), &settings)
	for _, s := range settings {
		if s.ID == "docker.io/u/custom:1" {
			t.Fatalf("deleted image still in settings: %q", raw)
		}
	}
}

func TestOrderPreserved(t *testing.T) {
	d := newImageTestDB(t)
	_ = SaveImageConfigs(d, []ImageConfigItem{
		{ID: "docker.io/narwhalcloud/alpine:podman", Enabled: true},
		{ID: "docker.io/narwhalcloud/debian:podman", Enabled: true},
	})
	got := idsOf(BuildImageList(d, builtinImages()))
	if got[0] != "docker.io/narwhalcloud/alpine:podman" || got[1] != "docker.io/narwhalcloud/debian:podman" {
		t.Fatalf("order not preserved: %v", got)
	}
}
