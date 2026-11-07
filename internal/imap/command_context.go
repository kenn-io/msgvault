package imap

import (
	"context"

	"github.com/emersion/go-imap/v2/imapclient"
)

// runCommandContext interrupts only the transport captured by the caller. The
// caller holds Client.mu; cancellation must never take that mutex or LOGOUT.
// Both the command and the captured transport's decoder are joined on cancel.
func runCommandContext[T any](ctx context.Context, conn *imapclient.Client, command func() (T, error)) (T, error) {
	var zero T
	if conn == nil {
		return command() // pure command-policy tests have no transport
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return zero, err
	}
	type result struct {
		value T
		err   error
	}
	finished := make(chan result, 1)
	go func() { value, err := command(); finished <- result{value, err} }()
	select {
	case result := <-finished:
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			return zero, err
		}
		return result.value, result.err
	case <-ctx.Done():
		_ = conn.Close()
		<-finished
		return zero, ctx.Err()
	}
}

// CloseContext bounds quiescent cleanup after a read returns. It is not the
// interruptor for a command holding mu: runCommandContext performs that task.
func (c *Client) CloseContext(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn := c.conn
	if conn == nil {
		return nil
	}
	defer c.invalidateConnLocked(conn)
	_, err := runCommandContext(ctx, conn, func() (struct{}, error) {
		return struct{}{}, conn.Logout().Wait()
	})
	return err
}
