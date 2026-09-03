package store

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"

	internalmime "go.kenn.io/msgvault/internal/mime"
)

const (
	defaultListIDRepairBatchSize  = 500
	defaultListIDRepairHeaderSize = 64 << 10
	defaultListIDRepairRawSize    = 128 << 10
)

// ListIDRepairOptions bounds the offline List-Id repair. Apply is false by
// default, so callers must explicitly choose to update archived message facts.
type ListIDRepairOptions struct {
	Apply          bool
	BatchSize      int
	MaxHeaderBytes int
	// MaxRawBytes bounds the database BLOB prefix read for each row. MIME
	// headers whose compressed bytes do not fit this prefix are undecodable;
	// this deliberate tradeoff prevents large attachments from becoming a
	// repair-memory input.
	MaxRawBytes int
}

// ListIDRepairSummary records the complete repair pass. Changed counts rows
// whose stored value differs from the archived MIME header, including dry runs.
type ListIDRepairSummary struct {
	Scanned     int64
	Found       int64
	Changed     int64
	Undecodable int64
}

type listIDRepairRow struct {
	id          int64
	listID      sql.NullString
	rawData     []byte
	rawLength   int64
	rawFormat   string
	compression sql.NullString
}

type listIDRepairUpdate struct {
	row    listIDRepairRow
	listID sql.NullString
}

// RepairListIDs re-derives email List-Id values from archived MIME. It scans
// by primary-key batches, never contacts a provider, and advances the
// cache-visible derived-data revision once when an applied pass changes rows.
// The optional progress callback receives cumulative counts after each batch;
// in apply mode those intermediate counts are not durable until the pass
// commits its single maintenance transaction.
func (s *Store) RepairListIDs(
	ctx context.Context,
	options ListIDRepairOptions,
	progress func(ListIDRepairSummary),
) (ListIDRepairSummary, error) {
	if err := ctx.Err(); err != nil {
		return ListIDRepairSummary{}, err
	}
	batchSize, headerSize, rawSize, err := normalizedListIDRepairOptions(options)
	if err != nil {
		return ListIDRepairSummary{}, err
	}

	if options.Apply {
		return s.repairListIDsApply(ctx, batchSize, headerSize, rawSize, progress)
	}
	return s.repairListIDBatches(ctx, s.db, batchSize, headerSize, rawSize, nil, progress)
}

// repairListIDsApply keeps every keyset batch in one maintenance transaction.
// The cache-visible revision and every derived List-Id therefore become visible
// together at COMMIT; cancellation or any later-batch error rolls all of them
// back.
func (s *Store) repairListIDsApply(
	ctx context.Context,
	batchSize int,
	headerSize int,
	rawSize int,
	progress func(ListIDRepairSummary),
) (ListIDRepairSummary, error) {
	var summary ListIDRepairSummary
	var changed int64
	if s.listIDRepairBeforeApplyHook != nil {
		s.listIDRepairBeforeApplyHook()
	}
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if err := s.lockListIDRepairSQLiteWriter(ctx, tx); err != nil {
			return err
		}
		var err error
		summary, err = s.repairListIDBatches(ctx, tx, batchSize, headerSize, rawSize,
			func(ctx context.Context, updates []listIDRepairUpdate) (int64, error) {
				if s.listIDRepairAfterScanHook != nil {
					if err := s.listIDRepairAfterScanHook(ctx, tx, updates); err != nil {
						return 0, err
					}
				}
				batchChanged, err := s.applyListIDRepairBatch(ctx, tx, updates, rawSize)
				changed += batchChanged
				return batchChanged, err
			}, progress)
		if err != nil {
			return err
		}
		if changed == 0 {
			return nil
		}
		return s.bumpDerivedDataRevision(tx)
	})
	if err != nil {
		return ListIDRepairSummary{}, err
	}
	return summary, nil
}

// lockListIDRepairSQLiteWriter makes the repair's first SQLite statement a
// writer-lock acquisition. A deferred SQLite transaction that reads first can
// never upgrade after another WAL writer commits, so it must reserve the writer
// slot before keyset scanning. PostgreSQL returns an empty row-lock template
// here and keeps its per-row SELECT FOR UPDATE behavior in applyListIDRepairBatch.
func (s *Store) lockListIDRepairSQLiteWriter(ctx context.Context, tx *loggedTx) error {
	// content_changed_at is deliberately excluded from SQLite's
	// trg_messages_last_modified UPDATE OF scope. A self-assignment still
	// reserves the writer slot, while an id self-assignment would advance an
	// unrelated message's optimistic-CAS watermark on every idempotent repair.
	lock := s.dialect.RowWriterLockSQL("messages", "content_changed_at")
	if lock == "" {
		return nil
	}
	// RowWriterLockSQL takes one id placeholder. Replacing it with a subquery
	// retains the dialect's SQLite self-assignment convention while making the
	// first statement select and lock an arbitrary email row atomically.
	lock = strings.Replace(lock,
		"?", "(SELECT id FROM messages WHERE message_type = 'email' ORDER BY id LIMIT 1)", 1)
	if _, err := tx.ExecContext(ctx, lock); err != nil {
		return fmt.Errorf("lock List-Id repair SQLite writer: %w", err)
	}
	return nil
}

func (s *Store) repairListIDBatches(
	ctx context.Context,
	queryer contextRowsQuerier,
	batchSize int,
	headerSize int,
	rawSize int,
	apply func(context.Context, []listIDRepairUpdate) (int64, error),
	progress func(ListIDRepairSummary),
) (ListIDRepairSummary, error) {
	var summary ListIDRepairSummary
	var lastID int64
	hasCursor := false
	for {
		if err := ctx.Err(); err != nil {
			return ListIDRepairSummary{}, err
		}
		rows, err := s.nextListIDRepairBatch(ctx, queryer, lastID, hasCursor, batchSize, rawSize)
		if err != nil {
			return ListIDRepairSummary{}, err
		}
		if len(rows) == 0 {
			break
		}
		hasCursor = true

		updates := make([]listIDRepairUpdate, 0, len(rows))
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return ListIDRepairSummary{}, err
			}
			lastID = row.id
			summary.Scanned++
			listID, decodeErr := decodeListIDHeader(row, headerSize)
			if decodeErr != nil {
				summary.Undecodable++
				continue
			}
			if listID != "" {
				summary.Found++
			}
			desired := sql.NullString{String: listID, Valid: listID != ""}
			if equalNullStrings(row.listID, desired) {
				continue
			}
			if apply != nil {
				updates = append(updates, listIDRepairUpdate{row: row, listID: desired})
			} else {
				summary.Changed++
			}
		}

		if len(updates) > 0 {
			changed, err := apply(ctx, updates)
			if err != nil {
				return ListIDRepairSummary{}, err
			}
			summary.Changed += changed
		}
		if progress != nil {
			progress(summary)
		}
	}

	return summary, nil
}

func normalizedListIDRepairOptions(options ListIDRepairOptions) (int, int, int, error) {
	batchSize := options.BatchSize
	if batchSize == 0 {
		batchSize = defaultListIDRepairBatchSize
	}
	if batchSize < 0 {
		return 0, 0, 0, errors.New("List-Id repair batch size must be positive")
	}
	headerSize := options.MaxHeaderBytes
	if headerSize == 0 {
		headerSize = defaultListIDRepairHeaderSize
	}
	if headerSize < 1 {
		return 0, 0, 0, errors.New("List-Id repair header limit must be positive")
	}
	rawSize := options.MaxRawBytes
	if rawSize == 0 {
		rawSize = max(defaultListIDRepairRawSize, headerSize+1)
	}
	if rawSize < 1 {
		return 0, 0, 0, errors.New("List-Id repair raw limit must be positive")
	}
	return batchSize, headerSize, rawSize, nil
}

func (s *Store) nextListIDRepairBatch(
	ctx context.Context,
	queryer contextRowsQuerier,
	afterID int64,
	hasCursor bool,
	batchSize int,
	rawSize int,
) ([]listIDRepairRow, error) {
	where := "WHERE " + listIDRepairEmailMessagePredicate
	args := []any{rawSize}
	if hasCursor {
		where += " AND m.id > ?"
		args = append(args, afterID)
	}
	args = append(args, batchSize)
	rows, err := queryer.QueryContext(ctx, `
		SELECT m.id, m.list_id, `+s.dialect.BlobPrefixSQL("mr.raw_data")+`,
		       LENGTH(mr.raw_data), mr.raw_format, mr.compression
		FROM messages m
		JOIN message_raw mr ON mr.message_id = m.id
		`+where+`
		ORDER BY m.id
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list List-Id repair messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	batch := make([]listIDRepairRow, 0, batchSize)
	for rows.Next() {
		var row listIDRepairRow
		if err := rows.Scan(&row.id, &row.listID, &row.rawData, &row.rawLength, &row.rawFormat, &row.compression); err != nil {
			return nil, fmt.Errorf("scan List-Id repair message: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate List-Id repair messages: %w", err)
	}
	return batch, nil
}

const listIDRepairEmailMessagePredicate = "COALESCE(m.message_type, '') IN ('', 'email')"

func decodeListIDHeader(row listIDRepairRow, maxHeaderBytes int) (string, error) {
	if row.rawFormat != "mime" {
		return "", errors.New("unsupported raw format")
	}
	raw, err := readListIDRepairHeader(row.rawData, row.compression, maxHeaderBytes)
	if err != nil {
		return "", err
	}
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if err != nil {
		return "", fmt.Errorf("parse MIME headers: %w", err)
	}
	return internalmime.NormalizeListID(header.Get("List-Id")), nil
}

func readListIDRepairHeader(rawData []byte, compression sql.NullString, maxHeaderBytes int) ([]byte, error) {
	var reader io.Reader = bytes.NewReader(rawData)
	if compression.Valid && compression.String == "zlib" {
		zlibReader, err := zlib.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("open zlib MIME: %w", err)
		}
		reader = zlibReader
		defer func() { _ = zlibReader.Close() }()
	}

	bounded := bufio.NewReader(io.LimitReader(reader, int64(maxHeaderBytes)+1))
	var header bytes.Buffer
	for {
		line, err := bounded.ReadBytes('\n')
		if len(line) > 0 {
			if header.Len()+len(line) > maxHeaderBytes {
				return nil, errors.New("MIME headers exceed repair limit")
			}
			header.Write(line)
			if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
				return header.Bytes(), nil
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil, errors.New("MIME headers are incomplete")
		case err != nil:
			return nil, fmt.Errorf("read MIME headers: %w", err)
		}
	}
}

func (s *Store) applyListIDRepairBatch(
	ctx context.Context,
	tx *loggedTx,
	updates []listIDRepairUpdate,
	rawSize int,
) (int64, error) {
	var changed int64
	eligible := make([]listIDRepairUpdate, 0, len(updates))
	for _, update := range updates {
		var messageID int64
		err := tx.QueryRowContext(ctx, `
				SELECT m.id
				FROM messages m
				JOIN message_raw mr ON mr.message_id = m.id
				WHERE m.id = ?
				  AND `+listIDRepairEmailMessagePredicate+`
				  AND m.list_id IS DISTINCT FROM ?
				  AND mr.raw_format = ?
				  AND mr.compression IS NOT DISTINCT FROM ?
				  AND LENGTH(mr.raw_data) = ?
				  AND `+s.dialect.BlobPrefixSQL("mr.raw_data")+` = ?`+s.dialect.SelectForUpdate(),
			update.row.id, update.listID, update.row.rawFormat, update.row.compression,
			update.row.rawLength, rawSize, update.row.rawData,
		).Scan(&messageID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return 0, fmt.Errorf("lock List-Id repair message %d: %w", update.row.id, err)
		}
		eligible = append(eligible, update)
	}
	if len(eligible) > 0 && s.listIDRepairAfterFingerprintLockHook != nil {
		s.listIDRepairAfterFingerprintLockHook()
	}
	for _, update := range eligible {
		result, err := tx.ExecContext(ctx, `
				UPDATE messages
				SET list_id = ?
				WHERE id = ?
				  AND list_id IS DISTINCT FROM ?
				  AND EXISTS (
					SELECT 1 FROM message_raw mr
					WHERE mr.message_id = messages.id
					  AND mr.raw_format = ?
					  AND mr.compression IS NOT DISTINCT FROM ?
					  AND LENGTH(mr.raw_data) = ?
					  AND `+s.dialect.BlobPrefixSQL("mr.raw_data")+` = ?
				  )
			`, update.listID, update.row.id, update.listID,
			update.row.rawFormat, update.row.compression, update.row.rawLength,
			rawSize, update.row.rawData)
		if err != nil {
			return 0, fmt.Errorf("repair List-Id for message %d: %w", update.row.id, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count List-Id repair for message %d: %w", update.row.id, err)
		}
		if count != 1 {
			return 0, fmt.Errorf("List-Id repair message %d changed %d rows after lock", update.row.id, count)
		}
		changed += count
	}
	return changed, nil
}

func equalNullStrings(left, right sql.NullString) bool {
	return left.Valid == right.Valid && (!left.Valid || left.String == right.String)
}
