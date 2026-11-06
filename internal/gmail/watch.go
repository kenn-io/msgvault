package gmail

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

var watchTopicPattern = regexp.MustCompile(`^projects/[A-Za-z0-9][A-Za-z0-9.:-]{0,127}/topics/[A-Za-z][A-Za-z0-9._~+-]{0,254}$`)

// WatchResult describes a mailbox-wide Gmail watch. HistoryID is informational:
// neither this result nor a notification may replace the archive's sync cursor.
type WatchResult struct {
	HistoryID  string
	Expiration time.Time
}

type watchRequestError struct{ cause error }

func (e *watchRequestError) Error() string { return "gmail watch request failed" }
func (e *watchRequestError) Unwrap() error { return e.cause }

// Watch installs or renews mailbox-wide notifications on an existing Pub/Sub
// topic. The operator must grant Gmail publishing permission and use the same
// Google developer project as this client's OAuth application. Callers verify
// the source-bound authenticated profile before installing the watch.
func (c *Client) Watch(ctx context.Context, topic string) (*WatchResult, error) {
	if !watchTopicPattern.MatchString(topic) {
		return nil, errors.New("gmail watch requires a valid Pub/Sub topic resource")
	}
	body, err := json.Marshal(struct {
		TopicName string `json:"topicName"`
	}{topic})
	if err != nil {
		return nil, errors.New("encode gmail watch request")
	}
	data, err := c.request(ctx, OpWatch, http.MethodPost, "/users/"+url.PathEscape(c.userID)+"/watch", body)
	if err != nil {
		return nil, &watchRequestError{cause: err}
	}
	var response struct {
		HistoryID  string `json:"historyId"`
		Expiration string `json:"expiration"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, errors.New("invalid gmail watch response")
	}
	history, err := strconv.ParseUint(response.HistoryID, 10, 64)
	if err != nil || history == 0 {
		return nil, errors.New("invalid gmail watch history ID")
	}
	millis, err := strconv.ParseInt(response.Expiration, 10, 64)
	if err != nil || millis <= time.Now().UnixMilli() {
		return nil, errors.New("invalid or expired gmail watch expiration")
	}
	return &WatchResult{HistoryID: response.HistoryID, Expiration: time.UnixMilli(millis)}, nil
}
