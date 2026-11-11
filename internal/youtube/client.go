package youtube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	yt "google.golang.org/api/youtube/v3"
)

var ErrQuota = errors.New("YouTube quota exhausted")

type APIError struct {
	StatusCode int
	Reason     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("YouTube API HTTP %d (%s)", e.StatusCode, e.Reason)
}
func (e *APIError) Is(target error) bool {
	return target == ErrQuota && (e.Reason == "quotaExceeded" || e.Reason == "dailyLimitExceeded")
}

type Client struct {
	service *yt.Service
	api     *http.Client
}

func NewClient(ctx context.Context, clientID, clientSecret, refreshToken string) (*Client, error) {
	return newClient(ctx, clientID, clientSecret, refreshToken, google.Endpoint, "")
}

func newClient(ctx context.Context, clientID, clientSecret, refreshToken string, endpoint oauth2.Endpoint, baseURL string) (*Client, error) {
	for _, v := range []string{clientID, clientSecret, refreshToken} {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("YouTube OAuth credentials missing or invalid")
		}
	}
	transport := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, transport)
	cfg := oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: endpoint, Scopes: []string{yt.YoutubeForceSslScope}}
	api := oauth2.NewClient(ctx, cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}))
	api.Timeout = transport.Timeout
	api.CheckRedirect = transport.CheckRedirect
	opts := []option.ClientOption{option.WithHTTPClient(api), option.WithLogger(slog.New(slog.DiscardHandler))}
	if baseURL != "" {
		opts = append(opts, option.WithEndpoint(baseURL))
	}
	service, err := yt.NewService(ctx, opts...)
	if err != nil {
		return nil, errors.New("construct YouTube service")
	}
	return &Client{service: service, api: api}, nil
}

func safeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if apiErr, ok := errors.AsType[*googleapi.Error](err); ok {
		reason := "unknown"
		for _, item := range apiErr.Errors {
			if item.Reason == "quotaExceeded" || item.Reason == "dailyLimitExceeded" {
				return &APIError{apiErr.Code, item.Reason}
			}
		}
		for _, item := range apiErr.Errors {
			switch item.Reason {
			case "commentsDisabled", "commentNotFound", "videoNotFound", "invalidPageToken", "forbidden":
				reason = item.Reason
			}
			if reason != "unknown" {
				break
			}
		}
		return &APIError{apiErr.Code, reason}
	}
	if oauthErr, ok := errors.AsType[*oauth2.RetrieveError](err); ok && oauthErr.ErrorCode == "invalid_grant" {
		return errors.New("YouTube authorization failed (invalid_grant); reauthorize the refresh token")
	}
	if ctx.Err() != nil {
		return fmt.Errorf("YouTube request: %w", ctx.Err())
	}
	return errors.New("YouTube request failed")
}

type ownershipResult struct {
	uploads, name, cursor string
	requests              int
}

func (c *Client) ownedChannel(ctx context.Context, channelID, cursor string, budget int) (ownershipResult, error) {
	var found ownershipResult
	if !validID(channelID) {
		return found, errors.New("invalid YouTube channel ID")
	}
	for budget == 0 || found.requests < budget {
		result, err := c.service.Channels.List([]string{"id", "snippet", "contentDetails"}).Mine(true).MaxResults(50).PageToken(cursor).Context(ctx).Do()
		found.requests++
		found.cursor = cursor
		if err != nil {
			return found, safeError(ctx, err)
		}
		for _, ch := range result.Items {
			if ch != nil && ch.Id == channelID && ch.Snippet != nil && ch.ContentDetails != nil && ch.ContentDetails.RelatedPlaylists != nil && validID(ch.ContentDetails.RelatedPlaylists.Uploads) {
				found.uploads, found.name = ch.ContentDetails.RelatedPlaylists.Uploads, ch.Snippet.Title
				return found, nil
			}
		}
		cursor = result.NextPageToken
		found.cursor = cursor
		if cursor == "" {
			return found, errors.New("YouTube credentials do not own the configured channel")
		}
	}
	return found, nil
}
