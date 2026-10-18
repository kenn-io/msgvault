package loops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

const kataRowTimeout = 10 * time.Second
const kataStopMargin = 15 * time.Second

// Bridge has its own pass and budget; derivation never waits on Kata.
// The only remote write it can make is an idempotent lifecycle comment.
type Bridge struct {
	st        *store.Store
	connector KataConnector
	cfg       config.LoopsKataConfig
	now       func() time.Time
	gate      chan struct{}
}
type BridgeResult struct {
	Processed int `json:"processed"`
	Failed    int `json:"failed"`
}

func NewBridge(st *store.Store, connector KataConnector, cfg *config.LoopsKataConfig) (*Bridge, error) {
	if st == nil || connector == nil {
		return nil, errors.New("kata loop bridge requires a store and connector")
	}
	c := config.DefaultLoopsConfig().Kata
	if cfg != nil {
		c = *cfg
	}
	if c.Batch < 1 || c.ReopenWindow <= 0 {
		return nil, errors.New("invalid Kata loop bridge configuration")
	}
	return &Bridge{st: st, connector: connector, cfg: c, now: time.Now, gate: make(chan struct{}, 1)}, nil
}
func (b *Bridge) RunScheduled(ctx context.Context) error { _, err := b.RunOnce(ctx); return err }

func (b *Bridge) RunOnce(ctx context.Context) (result BridgeResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	select {
	case b.gate <- struct{}{}:
		defer func() { <-b.gate }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	now := b.now().UTC()
	var firstFailure error
	defer func() {
		if err != nil && firstFailure == nil {
			firstFailure = errors.New("kata bridge storage failed")
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, b.st.RecordLoopKataRunContext(recordCtx, now, firstFailure))
	}()
	// Preflight shares the work budget, preserving the same state-write margin
	// even if the connection waits until its context expires.
	workCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		workCtx, cancel = context.WithDeadline(ctx, deadline.Add(-kataStopMargin))
		defer cancel()
	}
	kata, connectErr := b.connector.Connect(workCtx)
	if connectErr != nil {
		firstFailure = errors.New("kata connection failed")
		return result, nil
	}
	if kata == nil {
		firstFailure = ErrKataUnavailable
		return result, nil
	}
	if ctx.Err() != nil {
		firstFailure = kataFailure(ctx.Err())
		return result, nil
	}
	if workCtx.Err() != nil {
		return result, nil
	}
	rows, err := b.st.LoopKataWorkContext(workCtx, now, b.cfg.ReopenWindow, b.cfg.Batch, b.cfg.CommentOnClose, b.cfg.CloseOnIssueClosed)
	if err != nil {
		return result, err
	}
	archiveUID, err := b.st.ArchiveUIDContext(workCtx)
	if err != nil {
		return result, err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			firstFailure = kataFailure(ctx.Err())
			break
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < kataRowTimeout+kataStopMargin {
			break
		}
		result.Processed++
		rowCtx, cancel := context.WithTimeout(workCtx, kataRowTimeout)
		failure := b.process(rowCtx, kata, row, archiveUID, now)
		cancel()
		if failure != nil {
			result.Failed++
			if firstFailure == nil {
				firstFailure = kataFailure(failure)
			}
		}
	}
	return result, nil
}

func (b *Bridge) process(ctx context.Context, kata KataIssues, row store.OpenLoop, archiveUID string, now time.Time) error {
	if row.Kata == nil {
		return nil
	}
	link := *row.Kata
	issue, err := kata.Get(ctx, link.IssueUID)
	if err != nil {
		if errors.Is(err, taskclient.ErrNotFound) {
			return b.missing(ctx, link, now)
		}
		return b.fail(ctx, link, now, err)
	}
	link.IssueRef = issue.QualifiedRef
	link.IssueTitle = safeIssueTitle(issue.Title)
	link.IssueStatus = issue.Status
	link.IssueWebURL = personagenda.SafeWebURL(issue.WebURL)
	link.CheckedAt = &now
	link.UpdatedAt = now
	if err := b.st.UpdateLoopKataLinkContext(ctx, link); err != nil {
		return b.fail(ctx, link, now, err)
	}
	if b.cfg.CloseOnIssueClosed && (issue.Status == "closed" || issue.Status == "open") {
		if err := b.st.ReconcileLoopKataContext(ctx, row, issue.Status == "closed", now, b.cfg.ReopenWindow); err != nil {
			return b.fail(ctx, link, now, err)
		}
	}
	if b.cfg.CommentOnClose && row.ClosedReason != nil && *row.ClosedReason == "turn_flip" && link.ClosedNotifiedAt == nil {
		current, err := b.st.GetLoopKataLinkContext(ctx, row.ID)
		if errors.Is(err, store.ErrLoopKataLinkNotFound) {
			return nil
		}
		if err != nil {
			return b.fail(ctx, link, now, err)
		}
		if current.LinkToken != link.LinkToken {
			return nil
		}
		event, body := loopClosureComment(archiveUID, row)
		marker := "msgvault:lc:" + event[:16]
		posted := false
		for _, comment := range issue.CommentBodies {
			if strings.Contains(comment, marker) {
				posted = true
				break
			}
		}
		if !posted {
			if err := kata.Comment(ctx, link.IssueUID, event, body); err != nil {
				if errors.Is(err, taskclient.ErrNotFound) {
					return b.missing(ctx, link, now)
				}
				return b.fail(ctx, link, now, err)
			}
		}
		link.ClosedNotifiedAt = &now
	}
	link.Attempts = 0
	link.NextAttemptAt = &now
	link.LastError = ""
	if err := b.st.UpdateLoopKataLinkContext(ctx, link); err != nil {
		link.Attempts = row.Kata.Attempts
		return b.fail(ctx, link, now, err)
	}
	return nil
}
func (b *Bridge) missing(ctx context.Context, link store.LoopKataLink, now time.Time) error {
	link.IssueStatus = "missing"
	link.CheckedAt = &now
	link.LastError = "Kata issue is unavailable"
	link.Attempts = 0
	next := now.Add(kataBackoff(5))
	link.NextAttemptAt = &next
	link.UpdatedAt = now
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return b.st.UpdateLoopKataLinkContext(writeCtx, link)
}
func (b *Bridge) fail(ctx context.Context, link store.LoopKataLink, now time.Time, cause error) error {
	link.Attempts++
	next := now.Add(kataBackoff(link.Attempts))
	link.NextAttemptAt = &next
	link.LastError = kataFailure(cause).Error()
	link.UpdatedAt = now
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(cause, b.st.RecordLoopKataFailureContext(writeCtx, link))
}
func kataBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 30 * time.Minute
	case 4:
		return 2 * time.Hour
	default:
		return 6 * time.Hour
	}
}
func loopClosureComment(archiveUID string, row store.OpenLoop) (string, string) {
	messageID := int64(0)
	if row.ClosedMessageID != nil {
		messageID = *row.ClosedMessageID
	}
	event := kataevidence.Digest("msgvault.kata.loop-closed.v1", archiveUID, row.Kata.IssueUID, strconv.FormatInt(row.ID, 10), strconv.FormatInt(messageID, 10))
	date := ""
	if row.ClosedAt != nil {
		date = row.ClosedAt.UTC().Format("2006-01-02")
	}
	who := "The turn moved on"
	switch row.Kind {
	case ReplyOwed:
		who = "You replied on"
	case AwaitingReply:
		name := strings.TrimSpace(row.CounterpartyDisplayName)
		if name == "" && row.CounterpartyParticipantID != nil {
			name = fmt.Sprintf("participant %d", *row.CounterpartyParticipantID)
		}
		who = name + " replied on"
	}
	return event, fmt.Sprintf("%s %s (message %d); msgvault closed open loop %d.\n\nmsgvault:lc:%s", who, date, messageID, row.ID, event[:16])
}
