package imap

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v7"
	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/httpretry"
	"golang.org/x/time/rate"
)

var retryHint = regexp.MustCompile(`(?i)\bretry\s+in\s+([0-9]+)\s+seconds?\b`)

type retryFloorCaptureKey struct{}

// RetryFloorCapture records the latest provider-directed command deadline
// observed during one acquisition pass.
type RetryFloorCapture struct {
	mu        sync.Mutex
	notBefore time.Time
}

// WithRetryFloorCapture adds a private retry-floor capture to ctx. Child
// contexts inherit the capture; each acquisition should create its own.
func WithRetryFloorCapture(ctx context.Context) (context.Context, *RetryFloorCapture) {
	capture := &RetryFloorCapture{}
	return context.WithValue(ctx, retryFloorCaptureKey{}, capture), capture
}

// RetryNotBefore returns the latest captured provider deadline.
func (c *RetryFloorCapture) RetryNotBefore() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notBefore
}

type retryFloorError struct {
	err       error
	notBefore time.Time
}

func (e *retryFloorError) Error() string             { return e.err.Error() }
func (e *retryFloorError) Unwrap() error             { return e.err }
func (e *retryFloorError) RetryNotBefore() time.Time { return e.notBefore }

func withRetryFloor(err error, floor time.Time) error {
	if err == nil || !floor.After(time.Now()) {
		return err
	}
	var existing interface{ RetryNotBefore() time.Time }
	if errors.As(err, &existing) && !existing.RetryNotBefore().Before(floor) {
		return err
	}
	return &retryFloorError{err: err, notBefore: floor}
}

func maxRetryFloor(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

// commandRetryDelay accepts server-directed throttling only on a typed NO.
// A bare LIMIT has no delay, so use a one-second fallback.
func commandRetryDelay(err error) (time.Duration, bool) {
	var response *imap.Error
	if !errors.As(err, &response) || response.Type != imap.StatusResponseTypeNo {
		return 0, false
	}
	if match := retryHint.FindStringSubmatch(response.Text); match != nil {
		return httpretry.ParseRetryAfter(match[1], 0, time.Time{})
	}
	if strings.EqualFold(string(response.Code), string(imap.ResponseCodeLimit)) {
		return time.Second, true
	}
	return 0, false
}

// runCommand retries a single read command, never an entire operation with
// cache mutations or writes. Failed attempts' partial results are discarded.
// Each invocation has its own budget; existing transport reconnects may start
// one new invocation on the replacement connection.
func runCommand[T any](ctx context.Context, c *Client, command func() (T, error)) (T, error) {
	var zero T
	var capture *RetryFloorCapture
	if value, ok := ctx.Value(retryFloorCaptureKey{}).(*RetryFloorCapture); ok {
		capture = value
	}
	var observedFloor time.Time
	var deferredErr error
	result, err := backoff.Retry(ctx, func() (T, error) {
		// Yield a known provider floor before local pacing can hold the scheduler gate.
		// The locked check after pacing still fences a floor learned after this peek.
		if capture != nil {
			if err := ctx.Err(); err != nil {
				return zero, backoff.Permanent(err)
			}
			if floor := capture.RetryNotBefore(); time.Now().Before(floor) {
				if err := ctx.Err(); err != nil {
					return zero, backoff.Permanent(err)
				}
				return zero, backoff.Permanent(&retryFloorError{
					err:       errors.New("IMAP command deferred by provider retry deadline"),
					notBefore: floor,
				})
			}
		}
		if err := c.waitCommand(ctx); err != nil {
			return zero, backoff.Permanent(err)
		}
		if capture != nil {
			capture.mu.Lock()
			if err := ctx.Err(); err != nil {
				capture.mu.Unlock()
				return zero, backoff.Permanent(err)
			}
			if now := time.Now(); now.Before(capture.notBefore) {
				floor := capture.notBefore
				capture.mu.Unlock()
				return zero, backoff.Permanent(&retryFloorError{
					err:       errors.New("IMAP command deferred by provider retry deadline"),
					notBefore: floor,
				})
			}
		}
		conn := c.conn
		// runCommandContext can replace a completed response with ctx.Err; it
		// joins this callback before returning, so retain its raw result here.
		var callbackErr error
		var callbackAt time.Time
		result, err := runCommandContext(ctx, conn, func() (T, error) {
			result, err := command()
			callbackErr = err
			if err != nil {
				callbackAt = time.Now()
			}
			return result, err
		})
		if callbackErr != nil {
			delay, retry := commandRetryDelay(callbackErr)
			if retry {
				floor := callbackAt.Add(delay)
				observedFloor = maxRetryFloor(observedFloor, floor)
				if capture != nil {
					capture.notBefore = maxRetryFloor(capture.notBefore, floor)
				}
			}
		}
		if capture != nil {
			capture.mu.Unlock()
		}
		if ctx.Err() != nil {
			c.invalidateConnLocked(conn)
		}
		if err == nil {
			return result, nil
		}
		delay, retry := commandRetryDelay(err)
		if !retry {
			return zero, backoff.Permanent(err)
		}
		if capture != nil && delay > 0 {
			// Rapid acquisition shares the scheduled-work gate, so a positive
			// provider delay must yield to the scheduler instead of sleeping here.
			floor := maxRetryFloor(observedFloor, capture.RetryNotBefore())
			deferredErr = withRetryFloor(err, floor)
			c.logger.Debug("IMAP command limited; deferring acquisition until provider retry deadline",
				"delay", delay,
				"retry_not_before", floor,
			)
			return zero, backoff.Permanent(deferredErr)
		}
		return zero, backoff.RetryAfter(delay, err)
	}, backoff.WithMaxTries(4), backoff.WithMaxElapsedTime(0), backoff.WithNotify(func(_ error, delay time.Duration) {
		c.logger.Debug("IMAP command limited; waiting to retry", "delay", delay)
	}))
	if err != nil {
		if capture != nil {
			observedFloor = maxRetryFloor(observedFloor, capture.RetryNotBefore())
		}
		if ctx.Err() != nil {
			return zero, withRetryFloor(ctx.Err(), observedFloor)
		}
		if deferredErr != nil {
			return zero, withRetryFloor(deferredErr, observedFloor)
		}
		return zero, withRetryFloor(backoff.AsRetryError(err).LastErr, observedFloor)
	}
	return result, nil
}

// WithMaxCommandsPerMinute spaces this client's sync read commands evenly.
// Zero disables pacing. The limiter survives transport reconnects.
func WithMaxCommandsPerMinute(commands int) Option {
	return func(c *Client) {
		if commands > 0 {
			c.commandLimiter = rate.NewLimiter(rate.Limit(float64(commands)/60), 1)
		}
	}
}

func (c *Client) waitCommand(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.commandLimiter != nil {
		if err := c.commandLimiter.Wait(ctx); err != nil {
			return fmt.Errorf("wait for IMAP command: %w", err)
		}
	}
	return ctx.Err()
}

func (c *Client) selectCommand(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	// SELECT deselects the old mailbox even when the server rejects the new one.
	c.selectedMailbox = ""
	c.selectedUIDValidity = 0
	c.selectedNumMessages = 0
	c.clearQresyncCapture()
	return runCommand(ctx, c, func() (*imap.SelectData, error) { return c.conn.Select(mailbox, options).Wait() })
}

func (c *Client) listCommand(ctx context.Context, options *imap.ListOptions) ([]*imap.ListData, error) {
	return runCommand(ctx, c, func() ([]*imap.ListData, error) { return c.conn.List("", "*", options).Collect() })
}

func (c *Client) statusCommand(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	return runCommand(ctx, c, func() (*imap.StatusData, error) { return c.conn.Status(mailbox, options).Wait() })
}

func (c *Client) searchCommand(ctx context.Context, criteria *imap.SearchCriteria) (*imap.SearchData, error) {
	return runCommand(ctx, c, func() (*imap.SearchData, error) { return c.conn.UIDSearch(criteria, nil).Wait() })
}

func (c *Client) fetchCommand(ctx context.Context, uids imap.UIDSet, options *imap.FetchOptions) ([]*imapclient.FetchMessageBuffer, error) {
	return runCommand(ctx, c, func() ([]*imapclient.FetchMessageBuffer, error) { return c.conn.Fetch(uids, options).Collect() })
}

func (c *Client) enableCommand(ctx context.Context) (*imapclient.EnableData, error) {
	return runCommand(ctx, c, func() (*imapclient.EnableData, error) { return c.conn.Enable(imap.CapQResync).Wait() })
}
