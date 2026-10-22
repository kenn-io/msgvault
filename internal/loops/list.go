package loops

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"iter"
	"slices"
	"strings"
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
// CoverageIncomplete is set when List stopped after MaxListEvidenceComputations
// uncached messages; the loops come from the conversations read in full.
type Result struct {
	Loops              []Loop
	Truncated          bool
	SkippedUnparseable int
	CoverageIncomplete bool
}

// MaxListEvidenceComputations bounds uncached message classifications per request.
const MaxListEvidenceComputations = 1000
const evidenceVersion = 1

// EvidenceIncompleteError means an action needs another request to finish reading evidence.
type EvidenceIncompleteError struct{}

func (*EvidenceIncompleteError) Error() string {
	return "loops: evidence incomplete; retry to continue checking"
}

// ErrEvidenceIncomplete allows callers to retry after retained evidence advances.
var ErrEvidenceIncomplete = &EvidenceIncompleteError{}

// Store is what List, Done, and Snooze read and write; *store.Store
// implements it.
type Store interface {
	LoopConversationsContext(ctx context.Context, q store.LoopQuery) iter.Seq2[store.LoopConversation, error]
	LoopMarksContext(ctx context.Context) (map[int64]store.LoopMark, error)
	GetMessageBodyTextContext(ctx context.Context, messageID int64) (string, error)
	LoopOriginalEvidenceContext(ctx context.Context, messageID int64) (signals mime.AutomationSignals, hasFileAttachment bool, unreadable bool, err error)
	LoopMessageEvidenceContext(ctx context.Context, messageID int64, version int) (store.LoopMessageEvidence, string, bool, error)
	SaveLoopMessageEvidenceContext(ctx context.Context, messageID int64, version int, token string, evidence store.LoopMessageEvidence) error
	MessageConversationContext(ctx context.Context, messageID int64) (store.MessageConversationRef, error)
	MarkLoopDoneContext(ctx context.Context, anchorID int64, questions []store.LoopMessage) (store.LoopDecision, error)
	SnoozeLoopContext(ctx context.Context, anchorID int64, until time.Time, questions []store.LoopMessage) (store.LoopDecision, error)
}

type storeReader struct {
	ctx  context.Context
	st   Store
	left int
}

func (r *storeReader) Evidence(id int64) (store.LoopMessageEvidence, error) {
	e, token, hit, err := r.st.LoopMessageEvidenceContext(r.ctx, id, evidenceVersion)
	if err != nil || hit {
		return e, err
	}
	if r.left == 0 {
		return e, ErrEvidenceIncomplete
	}
	r.left--
	body, err := r.st.GetMessageBodyTextContext(r.ctx, id)
	if err != nil {
		return e, err
	}
	signals, attachment, unreadable, err := r.st.LoopOriginalEvidenceContext(r.ctx, id)
	if err != nil {
		return e, err
	}
	e = classify(body, signals, attachment, unreadable)
	return e, r.st.SaveLoopMessageEvidenceContext(r.ctx, id, evidenceVersion, token, e)
}

func classify(body string, signals mime.AutomationSignals, attachment, unreadable bool) store.LoopMessageEvidence {
	text := ownText(body)
	return store.LoopMessageEvidence{
		Empty: strings.TrimSpace(text) == "", Acknowledgement: isAcknowledgementOnly(text), Asks: asks(text),
		Automated:  signals.AutoSubmitted || signals.CalendarPart || signals.ReplyToNoReply || signals.PrecedenceBulk || signals.ListUnsubscribe,
		Attachment: attachment, Unreadable: unreadable,
	}
}

// find derives loops only from conversations whose detection completed.
func find(ctx context.Context, st Store, q store.LoopQuery, now time.Time, marks map[int64]store.LoopMark, found func(Loop) error) (int, error) {
	reader := &storeReader{ctx: ctx, st: st, left: MaxListEvidenceComputations}
	var unparsed int
	for conv, err := range st.LoopConversationsContext(ctx, q) {
		if err != nil {
			return 0, err
		}
		detection, err := Detect(conv, reader)
		if err != nil {
			return unparsed, err
		}
		unparsed += detection.Unparsed
		for _, loop := range detection.Loops {
			if loop, ok := openQuestions(loop, marks, now); ok {
				if err := found(loop); err != nil {
					return 0, err
				}
			}
		}
	}
	return unparsed, nil
}

// graceElapsed is whether a question has waited long enough to list: a reply
// you owe after 24 hours, and a reply you await after 72.
func graceElapsed(kind string, asked, now time.Time) bool {
	after := ReplyOwedAfter
	if kind == AwaitingReply {
		after = AwaitingAfter
	}
	return !now.Before(asked.Add(after))
}

// List returns unanswered questions. Decisions apply only to marked questions.
func List(ctx context.Context, st Store, opts Options, now time.Time) (Result, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	marks, err := st.LoopMarksContext(ctx)
	if err != nil {
		return Result{}, err
	}
	var kept []Loop
	unparsed, err := find(ctx, st, store.LoopQuery{PersonID: opts.PersonID}, now, marks,
		func(l Loop) error {
			if opts.PersonID != 0 && l.Counterparty.PersonID != opts.PersonID {
				return nil
			}
			at, _ := slices.BinarySearchFunc(kept, l, func(a, b Loop) int {
				return cmp.Or(a.OwedSince.Compare(b.OwedSince), cmp.Compare(a.Anchor.ID, b.Anchor.ID))
			})
			kept = slices.Insert(kept, at, l)
			kept = kept[:min(len(kept), opts.Limit+1)]
			return nil
		})
	if err != nil && ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	incomplete := errors.Is(err, ErrEvidenceIncomplete)
	if err != nil && !incomplete {
		return Result{}, err
	}
	result := Result{Loops: kept, SkippedUnparseable: unparsed, CoverageIncomplete: incomplete}
	if len(kept) > opts.Limit {
		result.Loops, result.Truncated = kept[:opts.Limit], true
	}
	return result, nil
}

// openQuestions applies exact decisions before choosing the visible anchor.
// List and actions use the same selection, and each question must be past
// its own grace period.
func openQuestions(loop Loop, marks map[int64]store.LoopMark, now time.Time) (Loop, bool) {
	loop.Questions = slices.DeleteFunc(loop.Questions, func(question store.LoopMessage) bool {
		mark := marks[question.ID]
		if !graceElapsed(loop.Kind, question.OccurredAt, now) {
			return true
		}
		return mark.State == store.LoopStateDone || (mark.SnoozedUntil != nil && now.Before(*mark.SnoozedUntil))
	})
	if len(loop.Questions) == 0 {
		return Loop{}, false
	}
	loop.Anchor = loop.Questions[0]
	loop.Summary, loop.OwedSince = summary(loop.Anchor), loop.Anchor.OccurredAt
	return loop, true
}

// ErrNotOpen reports a message that does not anchor an open loop.
var ErrNotOpen = errors.New("message does not anchor an open loop")

// Done marks the visible questions in the loop anchored at messageID done.
func Done(ctx context.Context, st Store, messageID int64, now time.Time) (store.LoopDecision, error) {
	loop, err := requireAnchor(ctx, st, messageID, now)
	if err != nil {
		return store.LoopDecision{}, err
	}
	return st.MarkLoopDoneContext(ctx, messageID, loop.Questions)
}

// Snooze hides the visible questions until the given time.
func Snooze(ctx context.Context, st Store, messageID int64, until, now time.Time) (store.LoopDecision, error) {
	loop, err := requireAnchor(ctx, st, messageID, now)
	if err != nil {
		return store.LoopDecision{}, err
	}
	return st.SnoozeLoopContext(ctx, messageID, until, loop.Questions)
}

// requireAnchor returns the loop List would show anchored at the message,
// past its grace period and read as it is now. A reply that
// arrived since the list was read makes the action fail instead of hiding a
// closed loop. Covered questions cannot anchor an action.
func requireAnchor(ctx context.Context, st Store, messageID int64, now time.Time) (Loop, error) {
	ref, err := st.MessageConversationContext(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return Loop{}, ErrNotOpen
	}
	if err != nil {
		return Loop{}, err
	}
	marks, err := st.LoopMarksContext(ctx)
	if err != nil {
		return Loop{}, err
	}
	var anchored Loop
	_, err = find(ctx, st, store.LoopQuery{ConversationID: ref.ConversationID}, now, marks,
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
