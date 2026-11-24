package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 显式清空的进程黑名单重启后必须保持为空；字段缺失才填默认名单。
func TestEmptyBlacklistSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Get().Patrol.Processes.Blacklist) == 0 {
		t.Fatal("字段缺失时应填入默认黑名单")
	}
	if err := m.Update(func(c *Config) { c.Patrol.Processes.Blacklist = []string{} }); err != nil {
		t.Fatal(err)
	}
	m, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(m.Get().Patrol.Processes.Blacklist); n != 0 {
		t.Fatalf("显式空名单重载后应保持为空，got %d", n)
	}
}
