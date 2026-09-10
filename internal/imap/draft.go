package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	DraftStateCreated              = "created"
	DraftStateAcceptedUnidentified = "accepted_unidentified"
	DraftStateRejected             = "rejected"
	DraftStateRemoteUnknown        = "remote_unknown"
	DraftStateCancelled            = "cancelled"
)

// DraftAppendResult records the provider outcome without retaining provider
// text in callers' error paths.
type DraftAppendResult struct {
	State       string
	UID         uint32
	UIDValidity uint32
	Code        string
}

// DraftAppendError carries a fixed reconciliation code.
type DraftAppendError struct {
	State string
	Code  string
	Err   error
}

func (e *DraftAppendError) Error() string { return e.Code }
func (e *DraftAppendError) Unwrap() error { return e.Err }

// ValidateDraftMailbox validates a configured literal mailbox name.
func ValidateDraftMailbox(mailbox string) error {
	if strings.TrimSpace(mailbox) == "" {
		return errors.New("mailbox must be nonblank")
	}
	if strings.ContainsAny(mailbox, "\x00\r\n") {
		return errors.New("mailbox contains control characters")
	}
	if !utf8.ValidString(mailbox) {
		return errors.New("mailbox must be valid UTF-8")
	}
	return nil
}

// AppendDraft sends one authenticated APPEND. It never selects, creates, or
// retries the mailbox operation.
func (c *Client) AppendDraft(ctx context.Context, mailbox string, raw []byte) (DraftAppendResult, error) {
	if err := ValidateDraftMailbox(mailbox); err != nil {
		return DraftAppendResult{State: DraftStateRejected, Code: "invalid_mailbox"}, err
	}
	if len(raw) == 0 {
		return DraftAppendResult{State: DraftStateRejected, Code: "invalid_message"}, errors.New("draft message is empty")
	}
	if err := ctx.Err(); err != nil {
		return DraftAppendResult{State: DraftStateCancelled, Code: "cancelled"}, err
	}
	var result DraftAppendResult
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		if !conn.Caps().Has(imaplib.CapUIDPlus) {
			result = DraftAppendResult{State: DraftStateRejected, Code: "uidplus_required"}
			return &DraftAppendError{State: result.State, Code: result.Code, Err: errors.New("IMAP server does not advertise UIDPLUS")}
		}
		if err := ctx.Err(); err != nil {
			result = DraftAppendResult{State: DraftStateCancelled, Code: "cancelled"}
			return err
		}
		command := conn.Append(mailbox, int64(len(raw)), &imaplib.AppendOptions{
			Flags: []imaplib.Flag{imaplib.FlagDraft},
		})
		finished := make(chan struct{})
		var data *imaplib.AppendData
		var appendErr error
		go func() {
			var written int64
			written, appendErr = io.Copy(command, bytes.NewReader(raw))
			if appendErr == nil && written != int64(len(raw)) {
				appendErr = io.ErrShortWrite
			}
			if appendErr == nil {
				appendErr = command.Close()
			}
			if appendErr == nil {
				data, appendErr = command.Wait()
			}
			close(finished)
		}()
		select {
		case <-finished:
		case <-ctx.Done():
			_ = conn.Close()
			<-finished
			result = DraftAppendResult{State: DraftStateRemoteUnknown, Code: "remote_unknown"}
			return &DraftAppendError{State: result.State, Code: result.Code, Err: io.ErrUnexpectedEOF}
		}
		if appendErr != nil {
			if isNetworkError(appendErr) {
				result = DraftAppendResult{State: DraftStateRemoteUnknown, Code: "remote_unknown"}
				return fmt.Errorf("append draft transport: %w", appendErr)
			}
			var statusErr *imaplib.Error
			if errors.As(appendErr, &statusErr) &&
				(statusErr.Type == imaplib.StatusResponseTypeNo || statusErr.Type == imaplib.StatusResponseTypeBad) {
				result = DraftAppendResult{State: DraftStateRejected, Code: "append_rejected"}
				return &DraftAppendError{State: result.State, Code: result.Code, Err: errors.New(result.Code)}
			}
			result = DraftAppendResult{State: DraftStateRemoteUnknown, Code: "remote_unknown"}
			return &DraftAppendError{State: result.State, Code: result.Code, Err: appendErr}
		}
		if data == nil || data.UID == 0 || data.UIDValidity == 0 {
			result = DraftAppendResult{State: DraftStateAcceptedUnidentified, Code: "accepted_unidentified"}
			return &DraftAppendError{State: result.State, Code: result.Code, Err: errors.New(result.Code)}
		}
		result = DraftAppendResult{
			State: DraftStateCreated, Code: "append_uidplus",
			UID: uint32(data.UID), UIDValidity: data.UIDValidity,
		}
		return nil
	})
	if err != nil {
		if appendErr, ok := errors.AsType[*DraftAppendError](err); ok {
			return result, appendErr
		}
		if ctx.Err() != nil {
			result = DraftAppendResult{State: DraftStateCancelled, Code: "cancelled"}
			return result, &DraftAppendError{State: result.State, Code: result.Code, Err: ctx.Err()}
		}
		if result.State == "" {
			result = DraftAppendResult{State: DraftStateRejected, Code: "connection_failed"}
		}
		return result, &DraftAppendError{State: result.State, Code: result.Code, Err: err}
	}
	return result, nil
}
