package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The diagnostic bounds both the number of point reads and each input to the
// existing stored-envelope rule. It never truncates a comparison into a match.
const (
	ftsDiagnosticChars      = 1_000_000
	ftsDiagnosticRecipients = 256
)

type FTSDiagnosticOptions struct {
	Cursor  string
	Limit   int
	Timeout time.Duration
}

type FTSDiagnosticEntry struct {
	MessageID     int64    `json:"message_id"`
	State         string   `json:"state"`
	Issues        []string `json:"issues"`
	Reason        string   `json:"reason,omitempty"`
	EmptyDocument bool     `json:"empty_document"`
}

// FTSDiagnosticReport describes one page. Complete means the selected ID
// interval was exhausted. Outcome is incomplete if any comparison is
// inconclusive, even when selection itself completed.
type FTSDiagnosticReport struct {
	Backend string `json:"backend"`
	// IndexingVersion is zero (not applicable) for SQLite and the current format
	// version for PostgreSQL. It does not describe whether an entry is present.
	IndexingVersion int                  `json:"indexing_version,omitempty"`
	ThroughID       int64                `json:"through_id"`
	NextCursor      string               `json:"next_cursor,omitempty"`
	Complete        bool                 `json:"complete"`
	Outcome         string               `json:"outcome"`
	Reason          string               `json:"reason,omitempty"`
	Checked         int                  `json:"checked"`
	Matched         int                  `json:"matched"`
	Defects         int                  `json:"defects"`
	Inconclusive    int                  `json:"inconclusive"`
	Entries         []FTSDiagnosticEntry `json:"entries"`
}

type ftsDiagnosticCursor struct {
	Format  int    `json:"format"`
	Archive string `json:"archive"`
	Backend string `json:"backend"`
	Version int    `json:"version"`
	After   int64  `json:"after"`
	Through int64  `json:"through"`
}

// DiagnoseFTSContext compares a bounded page with the document that
// IndexStoredMessageContext would build. Each message gets a read-only,
// repeatable-read snapshot. Pages and different messages are observations at
// different times, not a frozen archive. Only a completed observation advances
// the cursor; cancellation or a SQL failure leaves the interrupted row eligible.
func (s *Store) DiagnoseFTSContext(ctx context.Context, opts FTSDiagnosticOptions) (FTSDiagnosticReport, error) {
	r := FTSDiagnosticReport{Backend: "sqlite", Outcome: "incomplete", Entries: []FTSDiagnosticEntry{}}
	if s.IsPostgreSQL() {
		r.Backend, r.IndexingVersion = "postgres", CurrentFTSIndexingVersion
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Limit < 1 || opts.Limit > 1000 || opts.Timeout < time.Second || opts.Timeout > 5*time.Minute {
		return r, errors.New("FTS diagnostic requires limit 1..1000 and timeout 1s..5m")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		r.Reason = "canceled"
		if errors.Is(err, context.DeadlineExceeded) {
			r.Reason = "timeout"
		}
		return r, err
	}
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil || uid == "" {
		if ctx.Err() != nil {
			r.Reason = "interrupted"
			return r, ctx.Err()
		}
		return r, errors.New("FTS diagnostic cannot read archive identity")
	}
	c := ftsDiagnosticCursor{Format: 1, Archive: uid, Backend: r.Backend, Version: r.IndexingVersion}
	if opts.Cursor != "" {
		if len(opts.Cursor) > 2048 {
			return r, errors.New("invalid FTS diagnostic cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		var saved ftsDiagnosticCursor
		if err != nil || len(data) > 1024 || json.Unmarshal(data, &saved) != nil ||
			saved.Format != c.Format || saved.Archive != c.Archive || saved.Backend != c.Backend || saved.Version != c.Version ||
			saved.After < 0 || saved.Through < saved.After {
			return r, errors.New("invalid FTS diagnostic cursor for this archive and indexing format")
		}
		c = saved
	} else if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&c.Through); err != nil {
		if ctx.Err() != nil {
			r.Reason = "interrupted"
			return r, ctx.Err()
		}
		return r, errors.New("FTS diagnostic cannot bound message selection")
	}
	r.ThroughID = c.Through
	finish := func(reason string, cause error) (FTSDiagnosticReport, error) {
		r.Reason = reason
		if ctx.Err() != nil {
			r.Reason = "canceled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.Reason = "timeout"
			}
			cause = ctx.Err()
		}
		if !r.Complete {
			data, _ := json.Marshal(c)
			r.NextCursor = base64.RawURLEncoding.EncodeToString(data)
		}
		if r.Complete && r.Inconclusive == 0 && cause == nil {
			r.Outcome = "ok"
			if r.Defects > 0 {
				r.Outcome = "defects"
			}
		}
		return r, cause
	}
	if !s.fts5Available {
		return finish("fts_unavailable", nil)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages WHERE id > ? AND id <= ? ORDER BY id LIMIT ?`, c.After, c.Through, opts.Limit+1)
	if err != nil {
		return finish("selection_failed", errors.New("FTS diagnostic selection failed"))
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return finish("selection_failed", errors.New("FTS diagnostic selection failed"))
	}
	more := len(ids) > opts.Limit
	if more {
		ids = ids[:opts.Limit]
	}
	for _, id := range ids {
		entry, err := s.diagnoseFTSMessage(ctx, id)
		if err != nil {
			return finish("comparison_failed", errors.New("FTS diagnostic comparison failed; retry from next_cursor"))
		}
		if err := ctx.Err(); err != nil {
			return finish("interrupted", err)
		}
		r.Entries = append(r.Entries, entry)
		r.Checked++
		if len(entry.Issues) > 0 {
			r.Defects++
		}
		if entry.State == "inconclusive" {
			r.Inconclusive++
		}
		if entry.State == "matched" {
			r.Matched++
		}
		c.After = id
	}
	if err := ctx.Err(); err != nil {
		return finish("interrupted", err)
	}
	r.Complete = !more
	if more {
		return finish("page_limit", nil)
	}
	return finish("", nil)
}

func (s *Store) diagnoseFTSMessage(ctx context.Context, id int64) (FTSDiagnosticEntry, error) {
	e := FTSDiagnosticEntry{MessageID: id, State: "inconclusive", Issues: []string{}}
	// Own the transaction instead of joining any caller snapshot: a failed
	// PostgreSQL vector comparison must not poison another message's reads.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return e, err
	}
	defer func() { _ = tx.Rollback() }()
	q := boundQuerier{ctx: ctx, q: tx}
	var present bool
	var version sql.NullInt64
	query := `SELECT EXISTS(SELECT 1 FROM messages_fts WHERE rowid = m.id), NULL FROM messages m WHERE m.id = ?`
	if s.IsPostgreSQL() {
		query = `SELECT search_fts IS NOT NULL, indexing_version FROM messages WHERE id = ?`
	}
	err = q.QueryRow(query, id).Scan(&present, &version)
	if errors.Is(err, sql.ErrNoRows) {
		e.Reason = "message_disappeared"
		return e, nil
	}
	if err != nil {
		return e, err
	}
	if !present {
		e.Issues = append(e.Issues, "missing")
	}
	if s.IsPostgreSQL() && (!version.Valid || version.Int64 != CurrentFTSIndexingVersion) {
		e.Issues = append(e.Issues, "obsolete_version")
	}
	if !present {
		e.State = "defect"
		return e, nil
	}

	// Cap recipient work before the shared aggregate rule. Count only a bounded
	// indexed prefix, and inspect lengths rather than transferring private text.
	var count, chars int64
	lengths := []string{"mr.email_address", "mr.display_name", "p.email_address", "p.phone_number"}
	for i, col := range lengths {
		if !s.IsPostgreSQL() {
			// SQLite length(TEXT) stops at NUL; count all retained bytes.
			col = "CAST(" + col + " AS BLOB)"
		}
		lengths[i] = "COALESCE(length(" + col + "),0)"
	}
	err = q.QueryRow(`SELECT COUNT(*), COALESCE(SUM(n), 0) FROM (
		SELECT `+strings.Join(lengths, " + ")+` AS n
		FROM message_recipients mr LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? LIMIT ?) recipients`, id, ftsDiagnosticRecipients+1).Scan(&count, &chars)
	if err != nil {
		return e, err
	}
	if count > ftsDiagnosticRecipients || chars > ftsDiagnosticChars {
		e.Reason = "input_limit"
		return e, nil
	}
	aggregate := "GROUP_CONCAT"
	if s.IsPostgreSQL() {
		aggregate = "STRING_AGG"
	}
	envelope := storedFTSEnvelopeSQL(aggregate)
	doc := FTSDoc{MessageID: id}
	// Slice SQLite bytes so embedded NUL cannot hide retained text. Reading one
	// byte beyond the limit also rejects a UTF-8 rune cut at the boundary.
	boundedText := func(expr string) string {
		if !s.IsPostgreSQL() {
			return fmt.Sprintf("COALESCE(CAST(substr(CAST((%s) AS BLOB), 1, %d) AS TEXT), '')", expr, ftsDiagnosticChars+1)
		}
		return fmt.Sprintf("substr((%s), 1, %d)", expr, ftsDiagnosticChars+1)
	}
	// No body scan or list join: one primary-key read, bounded before transfer.
	err = q.QueryRow(`SELECT `+boundedText("COALESCE(body_text, '')")+` FROM message_bodies WHERE message_id = ?`, id).Scan(&doc.Body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e, err
	}
	columns := []string{envelope.subject, envelope.fromAddr, envelope.toAddrs, envelope.ccAddrs}
	for i, col := range columns {
		columns[i] = boundedText(col)
	}
	query = "SELECT " + strings.Join(columns, ", ") + " FROM messages m WHERE m.id = ?"
	if err := q.QueryRow(query, id).Scan(&doc.Subject, &doc.FromAddr, &doc.ToAddrs, &doc.CcAddrs); err != nil {
		return e, err
	}
	for _, value := range []string{doc.Body, doc.Subject, doc.FromAddr, doc.ToAddrs, doc.CcAddrs} {
		// PostgreSQL substr counts characters; this byte check is conservative for UTF-8.
		if len(value) > ftsDiagnosticChars {
			e.Reason = "input_limit"
			return e, nil
		}
	}
	emptyDoc := doc
	if s.IsPostgreSQL() {
		emptyDoc, _ = preparePostgresFTSDoc(doc)
	}
	e.EmptyDocument = emptyDoc.Subject == "" && emptyDoc.Body == "" && emptyDoc.FromAddr == "" && emptyDoc.ToAddrs == "" && emptyDoc.CcAddrs == ""
	var content, auxiliary bool
	if s.IsPostgreSQL() {
		vector, args := pgFTSVector(doc)
		err = q.QueryRow(`SELECT COALESCE(search_fts = (`+vector+`), FALSE), search_fts_fields IS NOT DISTINCT FROM $7::text[] FROM messages WHERE id = $1`, args...).Scan(&content, &auxiliary)
	} else {
		err = q.QueryRow(`SELECT COALESCE(subject,'') = ? AND COALESCE(body,'') = ? AND COALESCE(from_addr,'') = ? AND COALESCE(to_addr,'') = ? AND COALESCE(cc_addr,'') = ?, COALESCE(message_id = ?, FALSE) FROM messages_fts WHERE rowid = ?`,
			doc.Subject, doc.Body, doc.FromAddr, doc.ToAddrs, doc.CcAddrs, id, id).Scan(&content, &auxiliary)
	}
	if s.dialect.IsFTSValueTooLargeError(err) {
		e.Reason = "expected_vector_overflow"
		return e, nil
	}
	if err != nil {
		return e, err
	}
	if !content {
		e.Issues = append(e.Issues, "document_mismatch")
	}
	if !auxiliary {
		issue := "identity_mismatch"
		if s.IsPostgreSQL() {
			issue = "auxiliary_mismatch"
		}
		e.Issues = append(e.Issues, issue)
	}
	e.State = "matched"
	if len(e.Issues) > 0 {
		e.State = "defect"
	}
	return e, nil
}
