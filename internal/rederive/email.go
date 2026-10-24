package rederive

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Bump the version when parsing the same retained MIME produces different text.
const emailVersion = "email-v1"

// emailRetrySlots caps exact failed-row markers; later failures widen one
// overflow range, so retry state is at most emailRetrySlots+2 ledger rows.
const emailRetrySlots = 32

// Retry markers extend the pass's ledger key with colon-free suffixes, so a
// source merge clears them with its completion record:
//
//	#p  the last full walk finished and recorded every failed row
//	#f  one failed message ID
//	#r  an inclusive message ID range holding failed rows beyond the slots
const (
	emailRetryPhase = "#p"
	emailRetrySlot  = "#f"
	emailRetryRange = "#r"
)

// emailPass rebuilds text from retained MIME under ledger key base. With retry
// set, a source whose last full walk finished with failures retries only them.
func emailPass(base string, retry bool) Func {
	return func(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (*Summary, error) {
		w := &emailWalk{s: s, sourceID: sourceID, base: base, progress: progress, sum: &Summary{unchanged: true}}
		if !retry {
			return repairEmail(ctx, w)
		}
		pending, err := s.IsMigrationAppliedContext(ctx, base+emailRetryPhase, 1)
		if err != nil {
			return nil, err
		}
		if pending {
			return retryEmail(ctx, w)
		}
		return repairEmail(ctx, w)
	}
}

// emailWalk is one email pass's progress and the failures it has yet to persist.
type emailWalk struct {
	s        *store.Store
	sourceID int64
	base     string
	progress func(string)
	sum      *Summary
	slots    int
	lo, hi   int64
}

// repairEmail rebuilds text from retained MIME, including Gmail Chat messages.
// Each attempt starts at the first row; completed writes survive interruption.
// Clearing completion and retry state first means only a walk that recorded
// every failure can leave the retry phase marked.
func repairEmail(ctx context.Context, w *emailWalk) (*Summary, error) {
	start := time.Now()
	defer func() { w.sum.Duration = time.Since(start) }()
	if err := w.s.ForgetMigrationsContext(ctx, w.base); err != nil {
		return w.sum, err
	}
	if err := w.scan(ctx, 0, math.MaxInt64); err != nil {
		return w.sum, err
	}
	if w.sum.Errors == 0 {
		return w.sum, nil
	}
	if w.lo > 0 {
		if err := w.s.ReplaceMigrationContext(ctx, "", emailRetryRangeKey(w.base, w.lo, w.hi)); err != nil {
			return w.sum, err
		}
	}
	return w.sum, w.s.ReplaceMigrationContext(ctx, "", w.base+emailRetryPhase)
}

// retryEmail repairs the failed rows a finished full walk recorded. A marker
// is removed only after its rows resolve; the phase goes last, so runPass
// records completion only after every marker resolved.
func retryEmail(ctx context.Context, w *emailWalk) (*Summary, error) {
	start := time.Now()
	defer func() { w.sum.Duration = time.Since(start) }()
	w.slots = emailRetrySlots
	if err := w.retrySlots(ctx); err != nil {
		return w.sum, err
	}
	if err := w.retryRange(ctx); err != nil {
		return w.sum, err
	}
	if w.sum.Errors > 0 {
		return w.sum, nil
	}
	return w.sum, w.s.ReplaceMigrationContext(ctx, w.base+emailRetryPhase, "")
}

// scan repairs each retained MIME row with afterID < id <= throughID.
func (w *emailWalk) scan(ctx context.Context, afterID, throughID int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := w.s.ScanArchivedEmailContext(ctx, w.sourceID, afterID, throughID)
		if err != nil {
			return err
		}
		if item == nil {
			return ctx.Err()
		}
		afterID = item.MessageID
		failed, err := w.repairRow(ctx, item)
		if err == nil && failed {
			err = w.record(ctx, item.MessageID)
		}
		if err != nil {
			return err
		}
	}
}

// record persists a failed row as an exact marker while slots remain, then
// widens the overflow range the walk writes once it finishes.
func (w *emailWalk) record(ctx context.Context, id int64) error {
	if w.slots < emailRetrySlots {
		w.slots++
		return w.s.ReplaceMigrationContext(ctx, "", fmt.Sprintf("%s%s%016x", w.base, emailRetrySlot, id))
	}
	if w.lo == 0 {
		w.lo = id
	}
	w.hi = id
	return nil
}

// retrySlots reads only the exact failed IDs. A row that no longer belongs to
// the source resolves its marker.
func (w *emailWalk) retrySlots(ctx context.Context) error {
	prefix := w.base + emailRetrySlot
	for key := ""; ; {
		next, err := w.s.NextMigrationKeyContext(ctx, prefix, key)
		if err != nil || next == "" {
			return err
		}
		key = next
		id, ok := parseEmailRetryID(strings.TrimPrefix(key, prefix))
		if !ok {
			return w.malformed(ctx, key)
		}
		item, err := w.s.ScanArchivedEmailContext(ctx, w.sourceID, id-1, id)
		failed := false
		if err == nil && item != nil {
			failed, err = w.repairRow(ctx, item)
		}
		if err == nil && !failed {
			err = w.s.ReplaceMigrationContext(ctx, key, "")
		}
		if err != nil {
			return err
		}
	}
}

// retryRange rescans the overflow range and swaps it for the range of rows
// still failing only after the whole range was scanned.
func (w *emailWalk) retryRange(ctx context.Context) error {
	prefix := w.base + emailRetryRange
	key, err := w.s.NextMigrationKeyContext(ctx, prefix, "")
	if err != nil || key == "" {
		return err
	}
	lo, hi, ok := strings.Cut(strings.TrimPrefix(key, prefix), "-")
	first, firstOK := parseEmailRetryID(lo)
	last, lastOK := parseEmailRetryID(hi)
	if !ok || !firstOK || !lastOK || first > last {
		return w.malformed(ctx, key)
	}
	if err := w.scan(ctx, first-1, last); err != nil {
		return err
	}
	replacement := ""
	if w.lo > 0 {
		replacement = emailRetryRangeKey(w.base, w.lo, w.hi)
	}
	return w.s.ReplaceMigrationContext(ctx, key, replacement)
}

// malformed clears the retry phase so the next sync runs a full walk.
func (w *emailWalk) malformed(ctx context.Context, key string) error {
	if err := w.s.ReplaceMigrationContext(ctx, w.base+emailRetryPhase, ""); err != nil {
		return err
	}
	return fmt.Errorf("malformed email retry marker %q", key)
}

func emailRetryRangeKey(base string, lo, hi int64) string {
	return fmt.Sprintf("%s%s%016x-%016x", base, emailRetryRange, lo, hi)
}

// parseEmailRetryID accepts only the fixed-width hex IDs markers are written with.
func parseEmailRetryID(hex string) (int64, bool) {
	id, err := strconv.ParseInt(hex, 16, 64)
	if err != nil || id <= 0 || fmt.Sprintf("%016x", id) != hex {
		return 0, false
	}
	return id, true
}

// repairRow rebuilds one row. failed reports a write failure to retry; err is
// returned only for cancellation.
func (w *emailWalk) repairRow(ctx context.Context, item *store.ArchivedRawMessage) (bool, error) {
	sum := w.sum
	sum.MessagesScanned++
	parsed, err := parseArchivedEmail(item)
	if err != nil {
		sum.Undecodable++
		return false, nil //nolint:nilerr // Undecodable MIME is counted and left unchanged.
	}
	body := textutil.EnsureUTF8(parsed.GetBodyText())
	html := textutil.EnsureUTF8(parsed.BodyHTML)
	snippet := item.Snippet
	if snippet != "" && snippet == textutil.SnippetFromBody(item.BodyText) {
		snippet = textutil.SnippetFromBody(body)
	}
	changed, err := w.s.UpdateArchivedEmailTextContext(ctx, item,
		sql.NullString{String: body, Valid: body != ""}, sql.NullString{String: html, Valid: html != ""},
		sql.NullString{String: snippet, Valid: snippet != ""})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		sum.Errors++
		return true, nil
	}
	sum.unchanged = sum.unchanged && !changed
	if changed && item.BodyText != body {
		sum.BodiesRewritten++
	}
	if w.progress != nil {
		w.progress(fmt.Sprintf("%d scanned, %d bodies rewritten", sum.MessagesScanned, sum.BodiesRewritten))
	}
	return false, nil
}

func parseArchivedEmail(item *store.ArchivedRawMessage) (*mime.Message, error) {
	if item.RawDecodeError != nil {
		return nil, item.RawDecodeError
	}
	return mime.Parse(item.RawData)
}
