package loops

import (
	"context"
	"database/sql"
	"errors"
	"iter"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// Options selects which loops List returns. A zero Limit means the default.
type Options struct {
	PersonID int64
	Limit    int
}

// Result is the oldest-first slice of open loops. Truncated is set when more
// than Limit loops are open. SkippedUnparseable counts the stored originals
// the list could not read, which may hide a reply that settled a loop.
type Result struct {
	Loops              []Loop
	Truncated          bool
	SkippedUnparseable int
}

// Store is what List, Done, and Snooze read and write; *store.Store
// implements it.
type Store interface {
	LoopConversationsContext(ctx context.Context, q store.LoopQuery) iter.Seq2[store.LoopConversation, error]
	LoopMarksContext(ctx context.Context, now time.Time) (map[int64]store.LoopCutoff, error)
	GetMessageBodyTextContext(ctx context.Context, messageID int64) (string, error)
	LoopOriginalEvidenceContext(ctx context.Context, messageID int64) (signals mime.AutomationSignals, hasFileAttachment bool, unreadable bool, err error)
	MessageConversationContext(ctx context.Context, messageID int64) (store.MessageConversationRef, error)
	MarkLoopDoneContext(ctx context.Context, anchorID int64, through store.LoopMessage) (store.LoopDecision, error)
	SnoozeLoopContext(ctx context.Context, anchorID int64, until time.Time, through store.LoopMessage) (store.LoopDecision, error)
}

type storeReader struct {
	ctx context.Context
	st  Store
}

func (r storeReader) Body(id int64) (string, error) { return r.st.GetMessageBodyTextContext(r.ctx, id) }

func (r storeReader) Original(id int64) (mime.AutomationSignals, bool, bool, error) {
	return r.st.LoopOriginalEvidenceContext(r.ctx, id)
}

// find runs Detect over the conversations q selects, oldest first message
// first, and passes each loop that lists at now to found. Before it reads a
// conversation it asks done with that conversation's first message time, and
// it stops when done says so. It returns how many originals it could not read.
func find(ctx context.Context, st Store, q store.LoopQuery, now time.Time, done func(first time.Time) bool, found func(Loop) error) (int, error) {
	reader := storeReader{ctx: ctx, st: st}
	var unparsed int
	for conv, err := range st.LoopConversationsContext(ctx, q) {
		if err != nil {
			return 0, err
		}
		if done(conv.FirstAt) {
			break
		}
		detection, err := Detect(conv, reader)
		if err != nil {
			return 0, err
		}
		unparsed += detection.Unparsed
		if detection.Open && listsAt(detection.Loop, now) {
			if err := found(detection.Loop); err != nil {
				return 0, err
			}
		}
	}
	return unparsed, nil
}

// listsAt is whether a loop has waited long enough to list: a reply you owe
// after 24 hours, and a reply you await after 72.
func listsAt(l Loop, now time.Time) bool {
	after := ReplyOwedAfter
	if l.Kind == AwaitingReply {
		after = AwaitingAfter
	}
	return !now.Before(l.OwedSince.Add(after))
}

// List returns the open loops of conversations with a message in the last 30
// days, oldest first, without those you marked done or snoozed. A mark covers
// its conversation's questions up to the newest one when you made it, even
// once an older import moves the anchor; a newer question, or another one
// sent the same instant, lists the loop again, still dated by its anchor. A reply you
// owe lists after 24 hours, and a reply you await after 72. It reads the
// archive as it is now.
//
// Conversations come oldest first message first, and no loop starts before its
// conversation's first message. So List stops reading once it holds one more
// loop than the limit and the limit-th oldest is older than the next
// conversation's first message. A long list costs the loops it returns, not
// every conversation of the last 30 days.
func List(ctx context.Context, st Store, opts Options, now time.Time) (Result, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	marks, err := st.LoopMarksContext(ctx, now)
	if err != nil {
		return Result{}, err
	}
	var kept []Loop
	unparsed, err := find(ctx, st, store.LoopQuery{Since: now.Add(-Lookback), PersonID: opts.PersonID}, now,
		func(first time.Time) bool {
			return len(kept) > opts.Limit && kept[opts.Limit-1].OwedSince.Before(first)
		},
		func(l Loop) error {
			if opts.PersonID != 0 && l.Counterparty.PersonID != opts.PersonID {
				return nil
			}
			if cutoff, ok := marks[l.ConversationID]; ok {
				if !slices.ContainsFunc(l.Questions, func(ask store.LoopMessage) bool { return !store.LoopCutoffCovers(cutoff, ask) }) {
					return nil
				}
			}
			at, _ := slices.BinarySearchFunc(kept, l, func(a, b Loop) int {
				if c := a.OwedSince.Compare(b.OwedSince); c != 0 {
					return c
				}
				return int(a.Anchor.ID - b.Anchor.ID)
			})
			kept = slices.Insert(kept, at, l)
			kept = kept[:min(len(kept), opts.Limit+1)]
			return nil
		})
	if err != nil {
		return Result{}, err
	}
	result := Result{Loops: kept, SkippedUnparseable: unparsed}
	if len(kept) > opts.Limit {
		result.Loops, result.Truncated = kept[:opts.Limit], true
	}
	return result, nil
}

// ErrNotOpen reports a message that does not anchor an open loop.
var ErrNotOpen = errors.New("message does not anchor an open loop")

// Done marks the loop anchored at messageID done, through its newest question.
func Done(ctx context.Context, st Store, messageID int64, now time.Time) (store.LoopDecision, error) {
	loop, err := requireAnchor(ctx, st, messageID, now)
	if err != nil {
		return store.LoopDecision{}, err
	}
	return st.MarkLoopDoneContext(ctx, messageID, loop.LastAsk)
}

// Snooze hides the loop anchored at messageID until the given time, through
// its newest question. A done mark that covers that question stays.
func Snooze(ctx context.Context, st Store, messageID int64, until, now time.Time) (store.LoopDecision, error) {
	loop, err := requireAnchor(ctx, st, messageID, now)
	if err != nil {
		return store.LoopDecision{}, err
	}
	return st.SnoozeLoopContext(ctx, messageID, until, loop.LastAsk)
}

// requireAnchor returns the loop List would show anchored at the message: inside
// the lookback and past the grace period, read as it is now. A reply that
// arrived since the list was read makes the action fail instead of hiding a
// closed loop. Loops already marked done or snoozed still anchor.
func requireAnchor(ctx context.Context, st Store, messageID int64, now time.Time) (Loop, error) {
	ref, err := st.MessageConversationContext(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return Loop{}, ErrNotOpen
	}
	if err != nil {
		return Loop{}, err
	}
	var anchored Loop
	_, err = find(ctx, st, store.LoopQuery{Since: now.Add(-Lookback), ConversationID: ref.ConversationID}, now,
		func(time.Time) bool { return false },
		func(l Loop) error {
			if l.Anchor.ID == messageID {
				anchored = l
			}
			return nil
		})
	if err != nil {
		return Loop{}, err
	}
	if anchored.Anchor.ID != messageID {
		return Loop{}, ErrNotOpen
	}
	return anchored, nil
}
