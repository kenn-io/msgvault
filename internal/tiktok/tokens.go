package tiktok

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// TokenBundle uses absolute expiries so restarts cannot extend token lifetimes.
// Plain access tokens remain supported for externally managed authorization.
type TokenBundle struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitzero"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
}
type tokenState struct {
	Bootstrap      string      `json:"bootstrap"`
	Bundle         TokenBundle `json:"bundle"`
	RefreshPending bool        `json:"refresh_pending,omitempty"`
}

func stateKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// NewManagedClient shares rate state per app and rotated tokens per account.
// bootstrap is the value of the configured token environment variable.
func NewManagedClient(root, app, account, bootstrap, secret string) (*Client, error) {
	if !ValidID(app) || !ValidID(account) {
		return nil, errors.New("invalid TikTok app or account selector")
	}
	var initial TokenBundle
	if strings.HasPrefix(strings.TrimSpace(bootstrap), "{") {
		if json.Unmarshal([]byte(bootstrap), &initial) != nil {
			return nil, errors.New("TikTok token environment variable contains an invalid JSON bundle")
		}
		if initial.RefreshToken != "" && (initial.ExpiresAt.IsZero() || initial.RefreshExpiresAt.IsZero() || secret == "") {
			return nil, errors.New("TikTok refresh needs expires_at, refresh_expires_at and the app secret environment variable")
		}
	} else {
		initial.AccessToken = bootstrap
	}
	c, err := NewClient(initial.AccessToken)
	if err != nil {
		return nil, err
	}
	c.limiter = NewLimiter(filepath.Join(root, "rate-"+stateKey(app)+".json"))
	path := filepath.Join(root, "token-"+stateKey(app+"\x00"+account)+".json")
	c.token = func(ctx context.Context) (token string, err error) {
		err = withFileLock(ctx, path, func() error {
			var state tokenState
			if err := readState(path, &state); err != nil {
				return err
			}
			fingerprint := stateKey(bootstrap)
			if state.Bootstrap != fingerprint {
				state = tokenState{Bootstrap: fingerprint, Bundle: initial}
				if err := writeState(path, state); err != nil {
					return err
				}
			}
			bundle := state.Bundle
			if state.RefreshPending {
				return errors.New("previous TikTok token refresh did not complete; reconnect with a new token bundle")
			}
			if !bundle.ExpiresAt.IsZero() && time.Until(bundle.ExpiresAt) < 5*time.Minute {
				if bundle.RefreshToken == "" || !bundle.RefreshExpiresAt.After(time.Now()) {
					return errors.New("TikTok authorization expired; reconnect with a new token bundle")
				}
				// Persist intent before the rotating request. A crash or lost
				// response must not silently reuse a possibly consumed token.
				state.RefreshPending = true
				if err := writeState(path, state); err != nil {
					return err
				}
				dispatched := false
				refreshCtx := withDispatchHook(ctx, func() error { dispatched = true; return nil })
				rotated, err := c.Refresh(refreshCtx, app, secret, bundle.RefreshToken)
				if err != nil {
					if !dispatched || errors.Is(err, ErrRateLimited) {
						state.RefreshPending = false
						return errors.Join(err, writeState(path, state))
					}
					return err
				}
				state.Bundle = rotated
				state.RefreshPending = false
				if err := writeState(path, state); err != nil {
					return errors.New("persist rotated TikTok tokens failed; reconnect if authorization no longer works")
				}
				bundle = rotated
			}
			if bundle.AccessToken == "" || strings.ContainsAny(bundle.AccessToken, "\r\n") {
				return errors.New("invalid stored TikTok access token")
			}
			token = bundle.AccessToken
			return nil
		})
		return token, err
	}
	return c, nil
}

// Refresh is deliberately single-attempt: a lost response may have rotated
// the old refresh token. Callers must persist both returned tokens together.
func (c *Client) Refresh(ctx context.Context, app, secret, refresh string) (TokenBundle, error) {
	var result struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	err := c.request(ctx, "/tt_user/oauth2/refresh_token/", nil, map[string]string{"client_id": app, "client_secret": secret, "refresh_token": refresh, "grant_type": "refresh_token"}, &result, false, false)
	if err != nil {
		return TokenBundle{}, err
	}
	if result.AccessToken == "" || result.RefreshToken == "" || result.ExpiresIn <= 0 || result.ExpiresIn > 86400 || result.RefreshExpiresIn <= 0 || result.RefreshExpiresIn > 366*86400 {
		return TokenBundle{}, errors.New("invalid TikTok token refresh response; reconnect authorization")
	}
	now := time.Now()
	return TokenBundle{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, ExpiresAt: now.Add(time.Duration(result.ExpiresIn) * time.Second), RefreshExpiresAt: now.Add(time.Duration(result.RefreshExpiresIn) * time.Second)}, nil
}
