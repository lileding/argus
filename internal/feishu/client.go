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
	tokenURL       = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	replyURL       = "https://open.feishu.cn/open-apis/im/v1/messages/%s/reply"
	sendMsgURL     = "https://open.feishu.cn/open-apis/im/v1/messages"
	uploadImgURL   = "https://open.feishu.cn/open-apis/im/v1/images"
	downloadImgURL = "https://open.feishu.cn/open-apis/im/v1/images/%s"
	msgResourceURL = "https://open.feishu.cn/open-apis/im/v1/messages/%s/resources/%s?type=%s"
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

// ReplyRich sends a reply with arbitrary msg_type and pre-encoded content JSON.
func (c *Client) ReplyRich(messageID, msgType, contentJSON string) error {
	token, err := c.getToken()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	body := map[string]string{
		"content":  contentJSON,
		"msg_type": msgType,
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
		return fmt.Errorf("reply rich failed: status=%d body=%s", resp.StatusCode, respBody)
	}

	return nil
}

// SendMessageRich proactively sends a message with arbitrary msg_type and content.
func (c *Client) SendMessageRich(receiveIDType, receiveID, msgType, contentJSON string) error {
	token, err := c.getToken()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	body := map[string]string{
		"receive_id": receiveID,
		"content":    contentJSON,
		"msg_type":   msgType,
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

// UploadImage uploads an image to Feishu and returns the image_key.
func (c *Client) UploadImage(imageData []byte) (string, error) {
	token, err := c.getToken()
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}

	// Multipart form: image_type=message, image=<binary>
	var buf bytes.Buffer
	boundary := "----ArgusImageBoundary"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"image_type\"\r\n\r\n")
	buf.WriteString("message\r\n")
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"image\"; filename=\"image.png\"\r\n")
	buf.WriteString("Content-Type: image/png\r\n\r\n")
	buf.Write(imageData)
	buf.WriteString("\r\n--" + boundary + "--\r\n")

	req, err := http.NewRequest("POST", uploadImgURL, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("upload image error: code=%d msg=%s", result.Code, result.Msg)
	}

	return result.Data.ImageKey, nil
}

// DownloadImage downloads an image by image_key and returns raw bytes.
func (c *Client) DownloadImage(imageKey string) ([]byte, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	url := fmt.Sprintf(downloadImgURL, imageKey)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// DownloadMessageResource downloads a file/audio/video resource from a message.
// resourceType is "image", "file", or "audio".
func (c *Client) DownloadMessageResource(messageID, fileKey, resourceType string) ([]byte, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	url := fmt.Sprintf(msgResourceURL, messageID, fileKey, resourceType)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download resource: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
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
