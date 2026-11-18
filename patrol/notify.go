package patrol

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// sendTelegram 通过 Bot API 发送一条文本消息。失败仅记日志，不影响巡查流程。
func sendTelegram(token, chatID, text string) {
	if token == "" || chatID == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{
		"chat_id": chatID,
		"text":    text,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		log.Printf("[Patrol] telegram request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Patrol] telegram send: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		log.Printf("[Patrol] telegram send failed: status %d", resp.StatusCode)
	}
}
