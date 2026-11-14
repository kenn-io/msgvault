package beeper

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ArchiveChat sets the whole chat's inbox state. Send once: an interrupted
// response is uncertain and the caller must read back before reporting success.
func (c *Client) ArchiveChat(ctx context.Context, chatID string, archived bool) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("wait for Beeper archive rate limit: %w", err)
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	body := []byte(fmt.Sprintf(`{"archived":%t}`, archived))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chats/"+url.PathEscape(chatID)+"/archive", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("archive Beeper chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("archive Beeper chat: status %d", resp.StatusCode)
	}
	return nil
}
