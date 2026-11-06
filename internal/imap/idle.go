package imap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/oauth2"
)

var (
	ErrIdleUnsupported    = errors.New("IMAP server does not support IDLE")
	ErrIdleAuthentication = errors.New("IMAP watcher authentication or configuration failed")
	ErrIdleDisconnected   = errors.New("IMAP watcher connection interrupted")
)

// WatchInbox watches INBOX on a dedicated, read-only client. wake must return
// promptly; it only admits reconciliation, never performs ingestion here.
// Unilateral data is a hint, never a cursor or deletion instruction. The client
// is hard-closed before returning, including on cancellation or stalled DONE.
func (c *Client) WatchInbox(ctx context.Context, wake func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil || wake == nil {
		return ErrIdleAuthentication
	}
	hints := make(chan struct{}, 1)
	c.watchHint = func() {
		select {
		case hints <- struct{}{}:
		default:
		}
	}
	defer func() { c.watchHint = nil }()
	setupCtx, cancelSetup := context.WithTimeout(ctx, 30*time.Second)
	err := c.connect(setupCtx)
	cancelSetup()
	if err != nil {
		return idleResult(ctx, err, true)
	}
	conn := c.conn
	defer func() {
		// Ordinary Client.Close issues LOGOUT, which must never contend with
		// IDLE's command encoder. Closing the transport also joins its decoder.
		_ = conn.Close()
		c.conn = nil
		c.selectedMailbox = ""
		c.selectedUIDValidity = 0
	}()
	var caps imap.CapSet
	err = idleBounded(ctx, 30*time.Second, conn, func() error {
		var err error
		caps, err = conn.Capability().Wait()
		if err != nil {
			return fmt.Errorf("query IMAP IDLE capabilities: %w", err)
		}
		return nil
	})
	if err != nil {
		return idleResult(ctx, err, false)
	}
	if !caps.Has(imap.CapIdle) {
		return ErrIdleUnsupported
	}
	err = idleBounded(ctx, 30*time.Second, conn, func() error {
		_, err := conn.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("select read-only IMAP inbox: %w", err)
		}
		return nil
	})
	if err != nil {
		return idleResult(ctx, err, false)
	}
	doneTimeout := c.idleDoneTimeout
	if doneTimeout == 0 {
		doneTimeout = 5 * time.Second
	}
	renewInterval := c.idleRenewInterval
	if renewInterval == 0 {
		renewInterval = 20 * time.Minute
	}
	for {
		var idle *imapclient.IdleCommand
		err := idleBounded(ctx, 30*time.Second, conn, func() error {
			var err error
			idle, err = conn.Idle()
			if err != nil {
				return fmt.Errorf("start IMAP IDLE: %w", err)
			}
			return nil
		})
		if err != nil {
			return idleResult(ctx, err, false)
		}
		// Read-only selection can itself emit EXISTS; readiness gives one
		// explicit startup/reconnect catchup regardless of unsolicited data.
		select {
		case <-hints:
		default:
		}
		wake()
		renew := time.NewTimer(renewInterval)
		listening := true
		for listening {
			select {
			case <-ctx.Done():
				listening = false
			case <-conn.Closed():
				listening = false
			case <-renew.C:
				listening = false
			case <-hints:
				wake()
			}
		}
		renew.Stop()
		// Give DONE its own bounded grace period even after caller cancellation.
		// The timer hard-closes a peer that ignores DONE or stops reading it.
		err = idleBounded(context.Background(), doneTimeout, conn, func() error {
			if err := idle.Close(); err != nil {
				return fmt.Errorf("stop IMAP IDLE: %w", err)
			}
			if err := idle.Wait(); err != nil {
				return fmt.Errorf("wait for IMAP IDLE completion: %w", err)
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return idleResult(ctx, err, false)
		}
		select {
		case <-conn.Closed():
			return ErrIdleDisconnected
		default:
		}
	}
}

// idleBounded does not spawn a command goroutine that can outlive the watcher.
// Timeout/cancellation closes the socket, unblocking the actual protocol call.
func idleBounded(ctx context.Context, timeout time.Duration, conn *imapclient.Client, command func() error) error {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	closed := make(chan struct{})
	stop := context.AfterFunc(bounded, func() { _ = conn.Close(); close(closed) })
	err := command()
	if !stop() {
		<-closed
	}
	if bounded.Err() != nil {
		return bounded.Err()
	}
	return err
}

func idleResult(ctx context.Context, err error, authenticating bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var tokenFailure *oauth2.RetrieveError
	if errors.As(err, &tokenFailure) && tokenFailure.Response != nil &&
		(tokenFailure.Response.StatusCode == http.StatusTooManyRequests || tokenFailure.Response.StatusCode >= http.StatusInternalServerError) {
		return ErrIdleDisconnected
	}
	if protocol, ok := errors.AsType[*imap.Error](err); ok {
		switch protocol.Code {
		case imap.ResponseCodeUnavailable, imap.ResponseCodeServerBug, imap.ResponseCodeInUse, imap.ResponseCodeLimit:
			return ErrIdleDisconnected
		case imap.ResponseCodeAuthenticationFailed, imap.ResponseCodeAuthorizationFailed, imap.ResponseCodeExpired,
			imap.ResponseCodeNoPerm, imap.ResponseCodePrivacyRequired, imap.ResponseCodeNonExistent:
			return ErrIdleAuthentication
		default:
			if authenticating || protocol.Type == imap.StatusResponseTypeBad {
				return ErrIdleAuthentication
			}
			return ErrIdleDisconnected
		}
	}
	if authenticating && !isRetryableTransportError(err) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrIdleDisconnected) {
		return ErrIdleAuthentication
	}
	return ErrIdleDisconnected
}
