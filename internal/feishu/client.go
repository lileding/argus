package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"argus/internal/config"
)

const (
	tokenURL   = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	replyURL   = "https://open.feishu.cn/open-apis/im/v1/messages/%s/reply"
	sendMsgURL = "https://open.feishu.cn/open-apis/im/v1/messages"
)

// Client is a Feishu API client that handles token management and message sending.
type Client struct {
	cfg config.FeishuConfig

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

func NewClient(cfg config.FeishuConfig) *Client {
	return &Client{cfg: cfg}
}

// Reply sends a text reply to a specific message.
func (c *Client) Reply(messageID, text string) error {
	token, err := c.getToken()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	body := map[string]string{
		"content":  fmt.Sprintf(`{"text":%q}`, text),
		"msg_type": "text",
	}
	data, _ := json.Marshal(body)

	url := fmt.Sprintf(replyURL, messageID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("reply failed: status=%d body=%s", resp.StatusCode, respBody)
	}

	return nil
}

// SendMessage proactively sends a text message to a chat.
// receiveIDType is "chat_id" for group chats or "open_id" for private chats.
func (c *Client) SendMessage(receiveIDType, receiveID, text string) error {
	token, err := c.getToken()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	body := map[string]string{
		"receive_id": receiveID,
		"content":    fmt.Sprintf(`{"text":%q}`, text),
		"msg_type":   "text",
	}
	data, _ := json.Marshal(body)

	url := sendMsgURL + "?receive_id_type=" + receiveIDType
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send message failed: status=%d body=%s", resp.StatusCode, respBody)
	}

	return nil
}

func (c *Client) getToken() (string, error) {
	c.mu.RLock()
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry.Add(-10*time.Minute)) {
		token := c.accessToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	return c.refreshToken()
}

func (c *Client) refreshToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock.
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry.Add(-10*time.Minute)) {
		return c.accessToken, nil
	}

	body := map[string]string{
		"app_id":     c.cfg.AppID,
		"app_secret": c.cfg.AppSecret,
	}
	data, _ := json.Marshal(body)

	resp, err := http.Post(tokenURL, "application/json; charset=utf-8", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"` // seconds
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("token error: code=%d msg=%s", result.Code, result.Msg)
	}

	c.accessToken = result.TenantAccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(result.Expire) * time.Second)

	slog.Info("feishu token refreshed", "expires_in", result.Expire)
	return c.accessToken, nil
}
