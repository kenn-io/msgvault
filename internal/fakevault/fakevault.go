// Package fakevault generates synthetic msgvault archives for benchmarking
// and testing: a schema-valid msgvault.db plus a content-addressed
// attachments tree, sized by message count and an attachment byte budget.
//
// Generation is deterministic for a given seed: every message, participant,
// conversation, and attachment derives its own RNG from (seed, stream,
// index), so appending to an existing generated vault continues the same
// sequence. Wall-clock columns the schema owns (created_at defaults, the
// last_modified triggers) still vary between runs; determinism covers the
// content that backup cares about — row counts, text, and attachment bytes.
package fakevault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Options parameterizes one generation run.
type Options struct {
	// Dir is the vault directory; msgvault.db and attachments/ are created
	// inside it. Without Append the database must not already exist.
	Dir string
	// Messages is the number of messages to generate in this run.
	Messages int64
	// AttachmentBytes is the target total size of attachment content files
	// generated in this run. The realized total lands near it, not exactly
	// on it: sizes are drawn from a distribution and generation stops
	// attaching once the budget is spent.
	AttachmentBytes int64
	// Seed drives every random draw. Same seed + same run sequence =
	// same content.
	Seed uint64
	// Append extends an existing generated vault instead of creating one,
	// continuing message indexes where the previous run stopped. This is
	// the incremental-backup benchmarking mode.
	Append bool
	// Progress, if non-nil, is called after each batch with messages done
	// and the run total.
	Progress func(done, total int64)
}

// Summary reports what one run produced, measured from the database and
// this run's writes after generation completes.
type Summary struct {
	Messages        int64 // total rows in messages after the run
	Conversations   int64 // total rows in conversations after the run
	AttachmentRows  int64 // total rows in attachments after the run
	AttachmentBlobs int64 // content files written by this run
	AttachmentBytes int64 // bytes of content files written by this run
}

const (
	dbFileName = "msgvault.db"
	attDirName = "attachments"

	// timeLayout matches the RFC3339 UTC shape msgvault records for
	// message timestamps; MIN/MAX over sent_at compare correctly only
	// when every row uses one format.
	timeLayout = "2006-01-02T15:04:05Z"

	batchSize = 2000

	// meanAttachmentBytes is the expected value of drawAttachmentSize's
	// mixture; the per-message attachment rate is budget/(mean*messages).
	meanAttachmentBytes = 400_000
)

// Deterministic RNG streams: each entity kind draws from its own stream so
// adding draws to one kind never shifts another kind's sequence.
const (
	streamMessage = iota + 1
	streamAttachment
	streamConversation
	streamConvMember
	streamThumb
)

// messageEpoch is the fixed sent_at of message index 0; each message is 5
// minutes after the previous one (plus jitter), so 1M messages span about a
// decade. A fixed epoch keeps generated vaults comparable across runs.
var messageEpoch = time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)

type blobRef struct {
	hash      string
	size      int64
	mimeType  string
	mediaType string
	ext       string
	thumbHash string
}

type generator struct {
	opts      Options
	db        *sql.DB
	attDir    string
	start     int64 // first message index of this run
	total     int64 // total messages after this run
	convCount int64
	partCount int64
	attRate   float64 // expected attachments per message
	attBytes  int64   // attachment bytes written this run
	blobsNew  int64   // content files written this run
	blobs     []blobRef
	convSeen  map[int64]bool
	dirSeen   map[string]bool
}

// Generate creates or extends a synthetic vault under opts.Dir.
func Generate(ctx context.Context, opts Options) (*Summary, error) {
	if opts.Dir == "" {
		return nil, errors.New("fakevault: output directory is required")
	}
	if opts.Messages <= 0 {
		return nil, errors.New("fakevault: message count must be positive")
	}
	if opts.AttachmentBytes < 0 {
		return nil, errors.New("fakevault: attachment byte budget must not be negative")
	}
	dbPath := filepath.Join(opts.Dir, dbFileName)
	if err := checkTargetState(dbPath, opts.Append); err != nil {
		return nil, err
	}
	if err := initSchema(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=OFF&_foreign_keys=ON")
	if err != nil {
		return nil, fmt.Errorf("fakevault: opening database: %w", err)
	}
	defer func() { _ = db.Close() }()

	g := &generator{
		opts:     opts,
		db:       db,
		attDir:   filepath.Join(opts.Dir, attDirName),
		convSeen: map[int64]bool{},
		dirSeen:  map[string]bool{},
	}
	if err := g.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages").Scan(&g.start); err != nil {
		return nil, fmt.Errorf("fakevault: counting existing messages: %w", err)
	}
	g.total = g.start + opts.Messages
	g.convCount = max(g.total/40, 1)
	g.partCount = min(max(g.total/500, 50), 5000)
	g.attRate = float64(opts.AttachmentBytes) /
		(meanAttachmentBytes * float64(opts.Messages))

	if err := g.seedDimensions(ctx); err != nil {
		return nil, err
	}
	if err := g.generateMessages(ctx); err != nil {
		return nil, err
	}
	if err := g.finalize(ctx); err != nil {
		return nil, err
	}
	return g.summarize(ctx)
}

// checkTargetState enforces the create-vs-append contract before anything
// is written: creating over an existing vault or appending to a missing one
// is always a caller mistake.
func checkTargetState(dbPath string, appendMode bool) error {
	_, err := os.Stat(dbPath)
	switch {
	case err == nil && !appendMode:
		return fmt.Errorf(
			"fakevault: %s already exists (use append mode to extend it)", dbPath)
	case errors.Is(err, os.ErrNotExist) && appendMode:
		return fmt.Errorf("fakevault: cannot append: %s does not exist", dbPath)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("fakevault: checking %s: %w", dbPath, err)
	}
	return nil
}

// initSchema creates or migrates the database through the real store layer,
// so generated vaults always carry the schema shipped msgvault binaries
// expect — including index and trigger definitions.
func initSchema(dbPath string) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("fakevault: opening store: %w", err)
	}
	if err := st.InitSchema(); err != nil {
		_ = st.Close()
		return fmt.Errorf("fakevault: initializing schema: %w", err)
	}
	if err := st.Close(); err != nil {
		return fmt.Errorf("fakevault: closing store: %w", err)
	}
	return nil
}

// rng returns the deterministic RNG for one (stream, index) pair.
func (g *generator) rng(stream, index int64) *rand.Rand {
	return rand.New(rand.NewPCG(g.opts.Seed, uint64(stream)<<56^uint64(index))) //nolint:gosec // deterministic fake data, not crypto
}

// seedDimensions inserts the fixed-size dimension rows: sources, account
// identities, labels, and the participant pool. Everything uses explicit
// IDs and INSERT OR IGNORE so an append run can re-seed idempotently and
// grow the participant pool when the total message count crosses a
// threshold.
func (g *generator) seedDimensions(ctx context.Context) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fakevault: beginning dimension transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, s := range fakeSources {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO sources (id, source_type, identifier, display_name)
			 VALUES (?, ?, ?, ?)`, i+1, s.typ, s.identifier, s.name); err != nil {
			return fmt.Errorf("fakevault: inserting source %s: %w", s.identifier, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO account_identities (source_id, address)
			 VALUES (?, ?)`, i+1, s.identifier); err != nil {
			return fmt.Errorf("fakevault: inserting identity %s: %w", s.identifier, err)
		}
	}
	for i, name := range fakeLabels {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO labels (id, source_id, source_label_id, name, label_type)
			 VALUES (?, 1, ?, ?, ?)`, i+1, fmt.Sprintf("Label_%d", i+1), name,
			labelType(name)); err != nil {
			return fmt.Errorf("fakevault: inserting label %s: %w", name, err)
		}
	}
	if err := g.seedParticipants(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fakevault: committing dimensions: %w", err)
	}
	return nil
}

func (g *generator) seedParticipants(ctx context.Context, tx *sql.Tx) error {
	insPart, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO participants (id, email_address, display_name, domain)
		 VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("fakevault: preparing participant insert: %w", err)
	}
	defer func() { _ = insPart.Close() }()
	insIdent, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO participant_identifiers
		 (participant_id, identifier_type, identifier_value, is_primary)
		 VALUES (?, 'email', ?, TRUE)`)
	if err != nil {
		return fmt.Errorf("fakevault: preparing identifier insert: %w", err)
	}
	defer func() { _ = insIdent.Close() }()

	for i := range g.partCount {
		email, name, domain := participantIdentity(i)
		if _, err := insPart.ExecContext(ctx, i+1, email, name, domain); err != nil {
			return fmt.Errorf("fakevault: inserting participant %d: %w", i+1, err)
		}
		if _, err := insIdent.ExecContext(ctx, i+1, email); err != nil {
			return fmt.Errorf("fakevault: inserting identifier %d: %w", i+1, err)
		}
	}
	return nil
}

// generateMessages runs the batched message loop for this run's index range.
func (g *generator) generateMessages(ctx context.Context) error {
	for done := int64(0); done < g.opts.Messages; {
		n := min(int64(batchSize), g.opts.Messages-done)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := g.generateBatch(ctx, g.start+done, n); err != nil {
			return err
		}
		done += n
		if g.opts.Progress != nil {
			g.opts.Progress(done, g.opts.Messages)
		}
	}
	return nil
}

// batchStmts holds the per-transaction prepared statements one batch uses.
type batchStmts struct {
	msg, body, raw, recip, msgLabel, att *sql.Stmt
}

func prepareBatch(ctx context.Context, tx *sql.Tx) (*batchStmts, error) {
	var s batchStmts
	for _, p := range []struct {
		dst   **sql.Stmt
		query string
	}{
		{&s.msg, `INSERT INTO messages (id, conversation_id, source_id,
			source_message_id, rfc822_message_id, message_type, sent_at,
			received_at, sender_id, is_from_me, subject, snippet,
			size_estimate, has_attachments, attachment_count, archived_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`},
		{&s.body, `INSERT INTO message_bodies (message_id, body_text, body_html)
			VALUES (?, ?, ?)`},
		{&s.raw, `INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
			VALUES (?, ?, 'mime', 'zlib')`},
		{&s.recip, `INSERT OR IGNORE INTO message_recipients
			(message_id, participant_id, recipient_type) VALUES (?, ?, ?)`},
		{&s.msgLabel, `INSERT OR IGNORE INTO message_labels (message_id, label_id)
			VALUES (?, ?)`},
		{&s.att, `INSERT INTO attachments (message_id, filename, mime_type, size,
			content_hash, storage_path, media_type, thumbnail_hash, thumbnail_path)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`},
	} {
		stmt, err := tx.PrepareContext(ctx, p.query) //nolint:sqlclosecheck // closed by batchStmts.close via the caller's defer
		if err != nil {
			return nil, fmt.Errorf("fakevault: preparing batch statement: %w", err)
		}
		*p.dst = stmt
	}
	return &s, nil
}

func (s *batchStmts) close() {
	for _, stmt := range []*sql.Stmt{s.msg, s.body, s.raw, s.recip, s.msgLabel, s.att} {
		_ = stmt.Close()
	}
}

func (g *generator) generateBatch(ctx context.Context, start, n int64) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fakevault: beginning batch transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmts, err := prepareBatch(ctx, tx)
	if err != nil {
		return err
	}
	defer stmts.close()

	for idx := start; idx < start+n; idx++ {
		if err := g.insertMessage(ctx, tx, stmts, idx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fakevault: committing batch at %d: %w", start, err)
	}
	return nil
}

// insertMessage generates and inserts one message with its body, raw MIME,
// recipients, labels, and attachments. Draw order on the message RNG is
// fixed; changing it changes every seeded vault.
func (g *generator) insertMessage(ctx context.Context, tx *sql.Tx, stmts *batchStmts, idx int64) error {
	r := g.rng(streamMessage, idx)
	convID := 1 + int64(float64(g.convCount-1)*pow3(r.Float64()))
	if err := g.ensureConversation(ctx, tx, convID); err != nil {
		return err
	}
	srcID := 1 + convID%int64(len(fakeSources))
	src := fakeSources[srcID-1]
	sender := g.convMember(convID, r.IntN(g.convMemberCount(convID)))
	sentAt := messageEpoch.Add(time.Duration(idx)*5*time.Minute +
		time.Duration(r.IntN(240))*time.Second)
	sent := sentAt.Format(timeLayout)
	received := sentAt.Add(time.Duration(1+r.IntN(30)) * time.Second).Format(timeLayout)

	var subject any
	if src.msgType == "email" {
		subject = sentence(r, 3+r.IntN(6))
	}
	body := paragraphs(r, 1+r.IntN(4))
	msgID := idx + 1
	refs, err := g.collectAttachments(idx, r)
	if err != nil {
		return err
	}
	_, err = stmts.msg.ExecContext(ctx, msgID, convID, srcID,
		fmt.Sprintf("fake-%d", idx), fmt.Sprintf("<fake-%d@fake.local>", idx),
		src.msgType, sent, received, sender, r.Float64() < 0.15,
		subject, snippetOf(body), int64(len(body)), len(refs) > 0, len(refs), sent)
	if err != nil {
		return fmt.Errorf("fakevault: inserting message %d: %w", msgID, err)
	}
	if err := g.insertAttachmentRows(ctx, stmts, msgID, idx, refs); err != nil {
		return err
	}
	if err := g.insertBodyAndRaw(ctx, stmts, msgID, src.msgType, subject, body, sent, r); err != nil {
		return err
	}
	return g.insertRecipientsAndLabels(ctx, stmts, msgID, convID, src.msgType, r)
}

func (g *generator) insertBodyAndRaw(ctx context.Context, stmts *batchStmts,
	msgID int64, msgType string, subject any, body, sent string, r *rand.Rand) error {
	var html any
	if msgType == "email" && r.Float64() < 0.85 {
		html = htmlBody(body)
	}
	if _, err := stmts.body.ExecContext(ctx, msgID, body, html); err != nil {
		return fmt.Errorf("fakevault: inserting body %d: %w", msgID, err)
	}
	if msgType != "email" {
		return nil
	}
	raw, err := compressedMIME(msgID, subject, body, sent)
	if err != nil {
		return err
	}
	if _, err := stmts.raw.ExecContext(ctx, msgID, raw); err != nil {
		return fmt.Errorf("fakevault: inserting raw %d: %w", msgID, err)
	}
	return nil
}

func (g *generator) insertRecipientsAndLabels(ctx context.Context, stmts *batchStmts,
	msgID, convID int64, msgType string, r *rand.Rand) error {
	memberCount := g.convMemberCount(convID)
	for i, n := 0, 1+r.IntN(3); i < n; i++ {
		rtype := "to"
		if i > 0 {
			rtype = "cc"
		}
		member := g.convMember(convID, r.IntN(memberCount))
		if _, err := stmts.recip.ExecContext(ctx, msgID, member, rtype); err != nil {
			return fmt.Errorf("fakevault: inserting recipient for %d: %w", msgID, err)
		}
	}
	if msgType != "email" {
		return nil
	}
	for i, n := 0, 1+r.IntN(3); i < n; i++ {
		label := 1 + r.IntN(len(fakeLabels))
		if _, err := stmts.msgLabel.ExecContext(ctx, msgID, label); err != nil {
			return fmt.Errorf("fakevault: inserting label for %d: %w", msgID, err)
		}
	}
	return nil
}

// ensureConversation lazily inserts the conversation row and its member set
// the first time a message lands in it this run. Rows use INSERT OR IGNORE,
// so appends touching pre-existing conversations are no-ops.
func (g *generator) ensureConversation(ctx context.Context, tx *sql.Tx, convID int64) error {
	if g.convSeen[convID] {
		return nil
	}
	g.convSeen[convID] = true
	srcID := 1 + convID%int64(len(fakeSources))
	src := fakeSources[srcID-1]
	r := g.rng(streamConversation, convID)
	convType, title := "direct_chat", any(nil)
	if src.msgType == "email" {
		convType, title = "email_thread", any(sentence(r, 4+r.IntN(5)))
	} else if g.convMemberCount(convID) > 2 {
		convType, title = "group_chat", any(sentence(r, 2+r.IntN(3)))
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO conversations
		 (id, source_id, source_conversation_id, conversation_type, title)
		 VALUES (?, ?, ?, ?, ?)`,
		convID, srcID, fmt.Sprintf("conv-%d", convID), convType, title); err != nil {
		return fmt.Errorf("fakevault: inserting conversation %d: %w", convID, err)
	}
	for j := range g.convMemberCount(convID) {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO conversation_participants
			 (conversation_id, participant_id) VALUES (?, ?)`,
			convID, g.convMember(convID, j)); err != nil {
			return fmt.Errorf("fakevault: inserting members of %d: %w", convID, err)
		}
	}
	return nil
}

// convMemberCount and convMember derive a conversation's member set
// statelessly, so message generation can pick senders and recipients
// without carrying the set in memory or in the database.
func (g *generator) convMemberCount(convID int64) int {
	return 2 + g.rng(streamConversation, convID).IntN(5)
}

func (g *generator) convMember(convID int64, j int) int64 {
	return 1 + g.rng(streamConvMember, convID<<3|int64(j)).Int64N(g.partCount)
}

// finalize backfills the denormalized conversation stats and checkpoints
// the WAL so the finished vault is a compact, self-contained database file.
func (g *generator) finalize(ctx context.Context) error {
	if _, err := g.db.ExecContext(ctx, `UPDATE conversations SET
		message_count = (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = conversations.id),
		last_message_at = (SELECT MAX(m.sent_at) FROM messages m WHERE m.conversation_id = conversations.id),
		participant_count = (SELECT COUNT(*) FROM conversation_participants cp
			WHERE cp.conversation_id = conversations.id)`); err != nil {
		return fmt.Errorf("fakevault: updating conversation stats: %w", err)
	}
	if _, err := g.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("fakevault: checkpointing WAL: %w", err)
	}
	return nil
}

func (g *generator) summarize(ctx context.Context) (*Summary, error) {
	s := &Summary{AttachmentBlobs: g.blobsNew, AttachmentBytes: g.attBytes}
	for _, c := range []struct {
		dst   *int64
		query string
	}{
		{&s.Messages, "SELECT COUNT(*) FROM messages"},
		{&s.Conversations, "SELECT COUNT(*) FROM conversations"},
		{&s.AttachmentRows, "SELECT COUNT(*) FROM attachments"},
	} {
		if err := g.db.QueryRowContext(ctx, c.query).Scan(c.dst); err != nil {
			return nil, fmt.Errorf("fakevault: summary query: %w", err)
		}
	}
	return s, nil
}

func pow3(u float64) float64 { return u * u * u }

func labelType(name string) string {
	if name == "work" || name == "family" {
		return "user"
	}
	return "system"
}
