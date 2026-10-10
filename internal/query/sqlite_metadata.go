package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/search"
)

const metadataSearchTimeout = 10 * time.Second

func (e *SQLiteEngine) metadataSearchContext(ctx context.Context, q *search.Query) (context.Context, context.CancelFunc) {
	if len(q.TextTerms) > 0 && e.dialect.Rebind("?") == "?" {
		return context.WithTimeout(ctx, metadataSearchTimeout)
	}
	return ctx, func() {}
}

func metadataSearchError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("metadata search exceeded its deadline; narrow the date/account scope or use a longer term: %w", err)
	}
	return err
}

// hasMetadataFTS caches only a successful, complete readiness check. A failed
// probe (including cancellation) is retried, so setup can enable indexing on an
// engine that previously used the scan path.
func (e *SQLiteEngine) hasMetadataFTS(ctx context.Context) bool {
	if _, ok := e.dialect.(SQLiteQueryDialect); !ok {
		return false
	}
	e.metadataFTSMu.Lock()
	ready := e.metadataFTSReady
	e.metadataFTSMu.Unlock()
	if ready {
		return true
	}
	var version string
	if err := e.queryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='metadata_fts_version'`).Scan(&version); err != nil || version != "1" {
		return false
	}
	for _, table := range []string{"messages_metadata_fts", "participants_metadata_fts", "recipients_metadata_fts"} {
		var id int64
		err := e.queryRowContext(ctx, "SELECT rowid FROM "+table+" WHERE "+table+` MATCH '"msgvault-index-probe"' LIMIT 1`).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false
		}
	}
	e.metadataFTSMu.Lock()
	e.metadataFTSReady = true
	e.metadataFTSMu.Unlock()
	return true
}

func metadataTermCandidate(term string) (string, bool) {
	if !utf8.ValidString(term) || strings.ContainsRune(term, 0) {
		return "", false
	}
	folded := strings.ToLower(term)
	if utf8.RuneCountInString(folded) < 3 {
		return "", false
	}
	return `"` + strings.ReplaceAll(folded, `"`, `""`) + `"`, true
}

// A global candidate list can defeat a selective date/address/conversation
// index for common terms. Keep those established scoped scans. Source-only
// scopes still qualify: a collection may contain nearly the entire archive.
func metadataSearchScoped(q *search.Query, f MessageFilter) bool {
	return q.AfterDate != nil || q.BeforeDate != nil || len(q.FromAddrs)+len(q.ToAddrs)+len(q.CcAddrs)+len(q.BccAddrs) > 0 || len(q.ConversationIDs) > 0 ||
		f.After != nil || f.Before != nil || f.TimeRange.Period != "" || f.ConversationID != nil || f.Sender != "" || f.SenderName != "" || f.Recipient != "" || f.RecipientName != "" || f.Domain != ""
}

const metadataCandidateSQL = `m.id IN (
 SELECT rowid FROM messages_metadata_fts WHERE messages_metadata_fts MATCH ?
 UNION
 SELECT mr_candidate.message_id FROM message_recipients mr_candidate
 WHERE mr_candidate.participant_id IN (
  SELECT rowid FROM participants_metadata_fts WHERE participants_metadata_fts MATCH ?
 )
 UNION
 SELECT direct_candidate.id FROM messages direct_candidate
 WHERE direct_candidate.sender_id IN (
  SELECT rowid FROM participants_metadata_fts WHERE participants_metadata_fts MATCH ?
 )
 UNION
 SELECT alias_candidate.message_id FROM message_recipients alias_candidate
 WHERE alias_candidate.id IN (
  SELECT rowid FROM recipients_metadata_fts WHERE recipients_metadata_fts MATCH ?
 )
)`
