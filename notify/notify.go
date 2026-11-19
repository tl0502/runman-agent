// Package notify 提供 Agent 侧统一的 Telegram 通知通道：巡查告警、登录安全事件等
// 都通过 Notifier 发送，发送方只管给文本，通道开关与凭据由配置决定。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"runman-agent/config"
)

// Kind 通知类别。每个类别受对应的逐事件推送开关控制；登录类还叠加
// login_enabled 总门控（兼容存量配置）。
type Kind string

const (
	KindPatrol       Kind = "patrol"        // 巡查告警
	KindLoginOk      Kind = "login_ok"      // 面板登录成功
	KindAutoBan      Kind = "auto_ban"      // 连续失败触发自动封禁
	KindLoginBlocked Kind = "login_blocked" // 封禁中的 IP 仍在尝试登录
)

const telegramAPIBase = "https://api.telegram.org"

// Notifier 是全局通知出口：持有配置管理器，每次发送时读取最新配置
// （面板可随时改开关/凭据，无需重启）。
type Notifier struct {
	cfg *config.Manager
	// apiBase 仅为测试注入用，生产恒为 telegramAPIBase。
	apiBase string
}

// New 创建通知器。cfg 不能为 nil。
func New(cfg *config.Manager) *Notifier {
	return &Notifier{cfg: cfg, apiBase: telegramAPIBase}
}

// Send 异步发送一条通知（fire-and-forget）：未配置/开关关闭时静默跳过，
// 网络失败仅记日志，绝不影响调用方流程。
func (n *Notifier) Send(kind Kind, text string) {
	if n == nil || n.cfg == nil {
		return
	}
	conf := n.cfg.Get()
	if !conf.TelegramEnabled() {
		return
	}
	tg := conf.Telegram
	on := func(p *bool) bool { return p == nil || *p }
	switch kind {
	case KindPatrol:
		if !on(tg.PatrolNotify) {
			return
		}
	case KindLoginOk:
		if !tg.LoginNotifyOn() || !on(tg.LoginOkNotify) {
			return
		}
	case KindAutoBan:
		if !tg.LoginNotifyOn() || !on(tg.AutoBanNotify) {
			return
		}
	case KindLoginBlocked:
		if !tg.LoginNotifyOn() || !on(tg.LoginBlockedNotify) {
			return
		}
	}
	token, chatID := conf.Telegram.BotToken, conf.Telegram.ChatID
	if token == "" || chatID == "" {
		return
	}
	go func() {
		if err := n.postMessage(token, chatID, text); err != nil {
			log.Printf("[Notify] telegram send (%s): %v", kind, err)
		}
	}()
}

// SendTest 用当前配置同步发送一条测试消息并返回结果，供「发送测试」按钮
// 当场验证通道是否可用。不受 enabled 开关限制——配了凭据就能测，
// 否则「开关开着但收不到」和「根本发不出去」无法区分排查。
// token/chat 缺失直接报错，不发请求。
func (n *Notifier) SendTest() error {
	if n == nil || n.cfg == nil {
		return fmt.Errorf("通知器未初始化")
	}
	conf := n.cfg.Get()
	token, chatID := conf.Telegram.BotToken, conf.Telegram.ChatID
	if token == "" || chatID == "" {
		return fmt.Errorf("请先填写 Bot Token 和 Chat ID 并保存")
	}
	return n.postMessage(token, chatID, "✅ 测试消息\n通道配置正确，告警将推送到这里。")
}

// postMessage 同步调 Telegram Bot API 发送一条文本消息。
func (n *Notifier) postMessage(token, chatID, text string) error {
	body, _ := json.Marshal(map[string]string{
		"chat_id": chatID,
		"text":    text,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		n.apiBase+"/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram 返回状态码 %d", resp.StatusCode)
	}
	return nil
}
