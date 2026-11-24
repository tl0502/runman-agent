package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestVMConfigCreatedAtStable 验证 DB 锚点的两个关键语义：
// 1) 首次 INSERT 时 GORM 自动填 CreatedAt；
// 2) 重建走 SaveVMConfig(UPDATE) 时 CreatedAt 原样保留。
func TestVMConfigCreatedAtStable(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.AutoMigrate(&VMConfig{}); err != nil {
		t.Fatal(err)
	}
	d := &DB{orm: g}

	// 首次创建
	first := &VMConfig{VMID: "vm-1", CPU: 1, MemoryMB: 128, Status: "running"}
	if err := d.SaveVMConfig(first); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetVMConfig("vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be auto-filled on first insert")
	}
	anchor := got.CreatedAt

	// 模拟重建：驱动层再次 SaveVMConfig（同一主键，UPDATE）
	time.Sleep(5 * time.Millisecond)
	got.CPU = 2
	got.Image = "new-image"
	got.Status = "running"
	if err := d.SaveVMConfig(got); err != nil {
		t.Fatal(err)
	}
	after, err := d.GetVMConfig("vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !after.CreatedAt.Equal(anchor) {
		t.Fatalf("CreatedAt drifted across rebuild: %v -> %v", anchor, after.CreatedAt)
	}
}

// 异步续期晚于登出执行时，不能把已删除的会话写回去。
func TestTouchWebSessionDoesNotResurrect(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := d.SaveWebSession("h", "admin", now); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteWebSession("h"); err != nil {
		t.Fatal(err)
	}
	if err := d.TouchWebSession("h", now); err != nil {
		t.Fatal(err)
	}
	if n := len(d.ListWebSessions()); n != 0 {
		t.Fatalf("已撤销的会话被续期写回，got %d", n)
	}
}
