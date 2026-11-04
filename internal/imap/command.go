package imap

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/httpretry"
	"golang.org/x/time/rate"
)

var retryHint = regexp.MustCompile(`(?i)\bretry\s+in\s+([0-9]+)\s+seconds?\b`)

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
	result, err := backoff.Retry(ctx, func() (T, error) {
		if err := c.waitCommand(ctx); err != nil {
			return zero, backoff.Permanent(err)
		}
		result, err := command()
		if err == nil {
			return result, nil
		}
		delay, retry := commandRetryDelay(err)
		if !retry {
			return zero, backoff.Permanent(err)
		}
		return zero, backoff.RetryAfter(delay, err)
	}, backoff.WithMaxTries(4), backoff.WithMaxElapsedTime(0), backoff.WithNotify(func(_ error, delay time.Duration) {
		c.logger.Debug("IMAP command limited; waiting to retry", "delay", delay)
	}))
	if err != nil {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return zero, backoff.AsRetryError(err).LastErr
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
