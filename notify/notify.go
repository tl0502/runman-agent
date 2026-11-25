// Package notify 提供 Agent 侧统一的 Telegram 通知通道：巡查告警、登录安全事件等
// 都通过 Notifier 发送，发送方只管给文本，通道开关与凭据由配置决定。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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
	// entryAddr 返回本机入口地址（手动 host > public_ipv4 > 自动探测公网 IP），
	// 与面板「入口地址」展示、平台上报 entry_host 同口径。多面板共用同一个
	// Bot Token + Chat ID 时，靠它在消息首行区分消息来自哪台机器；可为 nil
	//（消息退化为仅站点名）。
	entryAddr func() string
	// apiBase 仅为测试注入用，生产恒为 telegramAPIBase。
	apiBase string
}

// New 创建通知器。cfg 不能为 nil；entryAddr 可为 nil。
func New(cfg *config.Manager, entryAddr func() string) *Notifier {
	return &Notifier{cfg: cfg, entryAddr: entryAddr, apiBase: telegramAPIBase}
}

// identityLine 返回消息首行的身份前缀："站点名 (入口地址)"；入口地址缺失时
// 仅站点名。站点名回落默认品牌名（Load 已把空值归一为 DefaultSiteName，
// 这里兜底防御直接构造的配置）。
func (n *Notifier) identityLine() string {
	site := ""
	if n != nil && n.cfg != nil {
		site = strings.TrimSpace(n.cfg.Get().SiteName)
	}
	if site == "" {
		site = config.DefaultSiteName
	}
	if n != nil && n.entryAddr != nil {
		if addr := strings.TrimSpace(n.entryAddr()); addr != "" {
			return site + " (" + addr + ")"
		}
	}
	return site
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
		// 身份首行在发送时刻取值：站点名/入口地址都是活配置，改完即生效。
		if err := n.postMessage(token, chatID, n.identityLine()+"\n"+text); err != nil {
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
	// 测试消息同样带身份首行：多面板共用一个 Bot 时，逐台点「发送测试」
	// 即可当场确认「我现在配的是哪台机器」。
	return n.postMessage(token, chatID, n.identityLine()+"\n✅ 测试消息\n通道配置正确，告警将推送到这里。")
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
		// net/http 的错误带完整请求 URL，而 Bot Token 就在路径里：脱敏后再外传。
		return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		// Telegram 在非 2xx 的响应体里给出 {"ok":false,"description":"..."}，
		// 只回状态码无法区分 token 错、chat_id 错、被限流、Bot 被停用。
		// 限长读取，避免异常响应体把内存/日志撑爆；token 仍保持脱敏。
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr struct {
			Description string `json:"description"`
		}
		desc := ""
		if json.Unmarshal(data, &apiErr) == nil {
			desc = strings.TrimSpace(apiErr.Description)
		}
		if desc == "" {
			desc = strings.TrimSpace(string(data))
		}
		if desc == "" {
			return fmt.Errorf("telegram 返回状态码 %d", resp.StatusCode)
		}
		return fmt.Errorf("telegram 返回状态码 %d: %s", resp.StatusCode,
			strings.ReplaceAll(desc, token, "***"))
	}
	// 少数错误（如 chat not found）也可能以 200 + ok=false 返回。
	var okResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if data, err := io.ReadAll(io.LimitReader(resp.Body, 4096)); err == nil {
		if json.Unmarshal(data, &okResp) == nil && !okResp.OK {
			desc := strings.TrimSpace(okResp.Description)
			if desc == "" {
				desc = "ok=false"
			}
			return fmt.Errorf("telegram 返回错误: %s",
				strings.ReplaceAll(desc, token, "***"))
		}
	}
	return nil
}
