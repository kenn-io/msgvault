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

// Limit the text and recipient data used for one comparison. Oversized input
// stays unverified: comparing a truncated prefix could report a false match.
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

// FTSDiagnosticReport describes one page. Complete means there are no more
// messages in the selected ID range; it says nothing about whether documents
// match. An inconclusive comparison keeps Outcome incomplete even when Complete
// is true.
type FTSDiagnosticReport struct {
	Backend string `json:"backend"`
	// IndexingVersion is the expected format, not a per-message version. Zero
	// means not applicable for SQLite; PostgreSQL uses its current format. This
	// field does not establish whether any search entry is present or valid.
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

// ftsDiagnosticCursor records the completed prefix of a fixed message ID range.
// Its archive, backend and format checks prevent accidental reuse on a different
// archive or indexing format. It is a continuation value, not authorization.
type ftsDiagnosticCursor struct {
	Format  int    `json:"format"`
	Archive string `json:"archive"`
	Backend string `json:"backend"`
	Version int    `json:"version"`
	After   int64  `json:"after"`
	Through int64  `json:"through"`
}

// DiagnoseFTSContext compares each search document with the one that
// IndexStoredMessageContext would build from stored message data. Metadata,
// content and search data for one message share a read-only transaction, so a
// concurrent update cannot mix old and new values. Different messages and pages
// can see later writes. The cursor advances only after recording a result; a
// canceled or failed read leaves the same message eligible for the next request.
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
		return r, errors.New("cannot determine which messages to check")
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
			return finish("comparison_failed", errors.New("message check failed; retry with --cursor and the reported continuation value"))
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
		// Record the result before advancing. A failed or canceled read above
		// must leave this message eligible when the operator resumes.
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
	// Give each message its own read-only transaction. This keeps its reads
	// consistent and lets a failed PostgreSQL comparison roll back without
	// leaving the next message's transaction in an error state.
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

	// Count at most one recipient beyond the limit before combining recipient
	// fields. Check lengths in SQL so oversized recipient data does not have to
	// leave the database.
	var count, chars int64
	lengths := []string{"mr.email_address", "mr.display_name", "p.email_address", "p.phone_number"}
	for i, col := range lengths {
		if !s.IsPostgreSQL() {
			// SQLite stops counting TEXT at a zero byte. Count the stored bytes
			// instead, so a suffix cannot escape the recipient limit.
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
	// SQLite text functions can stop at a zero byte. Read bytes so embedded
	// zeros do not hide the suffix. One extra byte lets the size check reject
	// oversized text instead of comparing a truncated prefix.
	boundedText := func(expr string) string {
		if !s.IsPostgreSQL() {
			return fmt.Sprintf("COALESCE(CAST(substr(CAST((%s) AS BLOB), 1, %d) AS TEXT), '')", expr, ftsDiagnosticChars+1)
		}
		return fmt.Sprintf("substr((%s), 1, %d)", expr, ftsDiagnosticChars+1)
	}
	// Read only this message's body by primary key. Apply the size limit in SQL
	// so the read neither scans other bodies nor transfers an unbounded value.
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
		// PostgreSQL substr counts characters, but this check counts bytes.
		// Multibyte text can therefore reach the limit sooner.
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
		// If even the expected search data is too large, we cannot establish
		// a match or infer why the stored representation looks the way it does.
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
