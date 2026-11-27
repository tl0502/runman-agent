package db

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// legacyPortForward 是加统计列之前的表结构，用于验证升级迁移。
type legacyPortForward struct {
	Protocol    string `gorm:"primaryKey"`
	HostPort    int    `gorm:"primaryKey"`
	VMID        string `gorm:"index"`
	GuestPort   int
	Description string
	BindFamily  int `gorm:"default:0"`
}

func (legacyPortForward) TableName() string { return "port_forwards" }

// TestPortForwardStatsMigration 模拟老版本库升级：表里没有 total_conns /
// conns_by_ip 两列，AutoMigrate 补列后必须能正常读回，不能让 NULL 扫进
// int64/string 报错——升级路径正是用户遇到「统计丢失」的那一步。
func TestPortForwardStatsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")

	raw, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.AutoMigrate(&legacyPortForward{}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Create(&legacyPortForward{
		Protocol: "tcp", HostPort: 20022, VMID: "vm-legacy", GuestPort: 22, Description: "ssh",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := raw.DB(); err == nil {
		_ = sqlDB.Close()
	}

	database, err := Init(path)
	if err != nil {
		t.Fatalf("Init on legacy db: %v", err)
	}

	pf, err := database.GetPortForward("tcp", 20022)
	if err != nil {
		t.Fatalf("GetPortForward: %v", err)
	}
	if pf == nil {
		t.Fatal("legacy row lost after migration")
	}
	if pf.TotalConns != 0 || pf.ConnsByIP != "" {
		t.Fatalf("legacy stats = (%d, %q), want (0, \"\")", pf.TotalConns, pf.ConnsByIP)
	}
	if pf.VMID != "vm-legacy" || pf.GuestPort != 22 || pf.Description != "ssh" {
		t.Fatalf("legacy row corrupted: %+v", pf)
	}
}

// TestSavePortForwardKeepsStats 规则重建（改描述/改内网端口/换 VM）时不能把
// 累计统计覆盖掉：SavePortForward 的 upsert 只更新规则字段。
func TestSavePortForwardKeepsStats(t *testing.T) {
	dir := t.TempDir()
	database, err := Init(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SavePortForward(&PortForward{
		Protocol: "tcp", HostPort: 20023, VMID: "vm-1", GuestPort: 22, Description: "ssh", BindFamily: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdatePortForwardStats("tcp", 20023, 42, `{"1.1.1.1":42}`); err != nil {
		t.Fatal(err)
	}

	// 同端口、改描述 + 改内网端口：统计必须留着。
	if err := database.SavePortForward(&PortForward{
		Protocol: "tcp", HostPort: 20023, VMID: "vm-1", GuestPort: 2222, Description: "sshd", BindFamily: 2,
	}); err != nil {
		t.Fatal(err)
	}

	pf, err := database.GetPortForward("tcp", 20023)
	if err != nil {
		t.Fatal(err)
	}
	if pf.TotalConns != 42 || pf.ConnsByIP != `{"1.1.1.1":42}` {
		t.Fatalf("stats wiped by rule update: (%d, %q)", pf.TotalConns, pf.ConnsByIP)
	}
	if pf.GuestPort != 2222 || pf.Description != "sshd" || pf.BindFamily != 2 {
		t.Fatalf("rule fields not updated: %+v", pf)
	}

	// 统计回写只动统计列。
	if err := database.UpdatePortForwardStats("tcp", 20023, 50, `{"1.1.1.1":50}`); err != nil {
		t.Fatal(err)
	}
	pf, _ = database.GetPortForward("tcp", 20023)
	if pf.TotalConns != 50 || pf.Description != "sshd" {
		t.Fatalf("stats update touched rule fields: %+v", pf)
	}
}

// TestGetPortForwardMissing 不存在的规则返回 nil, nil（不当作错误）。
func TestGetPortForwardMissing(t *testing.T) {
	database, err := Init(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	pf, err := database.GetPortForward("tcp", 12345)
	if err != nil || pf != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", pf, err)
	}
}
