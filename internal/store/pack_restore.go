package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

const (
	restorePackIndexDDL = `CREATE TABLE IF NOT EXISTS attachment_pack_index (
		blob_hash   TEXT PRIMARY KEY,
		pack_id     TEXT NOT NULL,
		pack_offset BIGINT NOT NULL,
		stored_len  BIGINT NOT NULL,
		raw_len     BIGINT NOT NULL,
		flags       INTEGER NOT NULL,
		crc32c      BIGINT NOT NULL
	)`
	restorePackIndexByPackDDL = `CREATE INDEX IF NOT EXISTS idx_attachment_pack_index_pack
		ON attachment_pack_index(pack_id)`
	restorePacksDDL = `CREATE TABLE IF NOT EXISTS attachment_packs (
		pack_id      TEXT PRIMARY KEY,
		entry_count  BIGINT NOT NULL,
		stored_bytes BIGINT NOT NULL,
		created_at   TEXT NOT NULL
	)`
)

// RestorePackCatalog replaces packed attachment authority in an unpublished
// restored SQLite database. It deliberately owns no Store or filesystem state.
type RestorePackCatalog struct {
	db *sql.DB
}

var _ packstore.RestoreCatalog = (*RestorePackCatalog)(nil)

// NewRestorePackCatalog creates only the packed-attachment tables and index
// required by restored authority. It never runs the application's broader
// schema initialization or migrations and does not take ownership of db.
func NewRestorePackCatalog(ctx context.Context, db *sql.DB) (*RestorePackCatalog, error) {
	if db == nil {
		return nil, errors.New("restore pack catalog: database is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var version string
	if err := db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&version); err != nil {
		return nil, fmt.Errorf("restore pack catalog requires SQLite: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin restore pack schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []struct {
		name string
		sql  string
	}{
		{name: "attachment pack index table", sql: restorePackIndexDDL},
		{name: "attachment pack lookup index", sql: restorePackIndexByPackDDL},
		{name: "attachment packs table", sql: restorePacksDDL},
	} {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return nil, fmt.Errorf("create restored %s: %w", statement.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit restore pack schema: %w", err)
	}
	return &RestorePackCatalog{db: db}, nil
}

// ReplaceRestoredPacks atomically replaces every pack record and selected
// mapping after proving each mapped hash is live in the restored snapshot.
// Attachment rows and paths remain untouched; they are the liveness authority.
func (c *RestorePackCatalog) ReplaceRestoredPacks(
	ctx context.Context,
	records []packstore.PackRecord,
	adoptions []packstore.Adoption,
) error {
	if c == nil || c.db == nil {
		return errors.New("restore pack catalog is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	authority, err := validateRestoredPackAuthority(records, adoptions)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restored pack replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, err := restoredAttachmentMembership(ctx, tx)
	if err != nil {
		return err
	}
	for _, adoption := range authority.adoptions {
		if _, ok := membership[adoption.Entry.Hash]; !ok {
			return fmt.Errorf("restore pack hash %s is not referenced by the restored snapshot", adoption.Entry.Hash)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_pack_index`); err != nil {
		return fmt.Errorf("clear restored attachment pack index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_packs`); err != nil {
		return fmt.Errorf("clear restored attachment packs: %w", err)
	}
	for _, record := range authority.records {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
			VALUES (?, ?, ?, ?)`, record.PackID, record.EntryCount, record.StoredBytes,
			record.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("insert restored attachment pack %s: %w", record.PackID, err)
		}
	}
	for _, adoption := range authority.adoptions {
		entry := adoption.Entry
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attachment_pack_index
			    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, entry.Hash.String(), entry.PackID,
			entry.Offset, entry.StoredLen, entry.RawLen, int64(entry.Flags), int64(entry.CRC32C)); err != nil {
			return fmt.Errorf("insert restored attachment pack mapping %s: %w", entry.Hash, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restored pack replacement: %w", err)
	}
	return nil
}

type restoredPackAuthority struct {
	records   []packstore.PackRecord
	adoptions []packstore.Adoption
}

func validateRestoredPackAuthority(
	records []packstore.PackRecord,
	adoptions []packstore.Adoption,
) (restoredPackAuthority, error) {
	authority := restoredPackAuthority{
		records:   append([]packstore.PackRecord(nil), records...),
		adoptions: make([]packstore.Adoption, len(adoptions)),
	}
	for i, adoption := range adoptions {
		authority.adoptions[i] = adoption
		authority.adoptions[i].OriginalHashes = append([]string(nil), adoption.OriginalHashes...)
	}
	recordsByID := make(map[string]packstore.PackRecord, len(records))
	for _, record := range authority.records {
		if err := record.Validate(); err != nil {
			return restoredPackAuthority{}, fmt.Errorf("validate restored pack record: %w", err)
		}
		if _, duplicate := recordsByID[record.PackID]; duplicate {
			return restoredPackAuthority{}, fmt.Errorf("duplicate restored pack record %s", record.PackID)
		}
		recordsByID[record.PackID] = record
	}
	seenHashes := make(map[packstore.Hash]struct{}, len(adoptions))
	selectedEntries := make(map[string]int64, len(records))
	selectedStored := make(map[string]int64, len(records))
	for _, adoption := range authority.adoptions {
		entry := adoption.Entry
		if err := entry.Validate(); err != nil {
			return restoredPackAuthority{}, fmt.Errorf("validate restored pack mapping: %w", err)
		}
		if entry.Flags&^uint8(pack.BlobCompressed) != 0 {
			return restoredPackAuthority{}, fmt.Errorf("restored pack mapping %s has unsupported flags %#x", entry.Hash, entry.Flags)
		}
		if _, duplicate := seenHashes[entry.Hash]; duplicate {
			return restoredPackAuthority{}, fmt.Errorf("duplicate restored pack mapping %s", entry.Hash)
		}
		seenHashes[entry.Hash] = struct{}{}
		record, ok := recordsByID[entry.PackID]
		if !ok {
			return restoredPackAuthority{}, fmt.Errorf("restored pack mapping %s names missing pack record %s", entry.Hash, entry.PackID)
		}
		selectedEntries[entry.PackID]++
		if selectedEntries[entry.PackID] > record.EntryCount {
			return restoredPackAuthority{}, fmt.Errorf("restored pack %s maps more selected entries than footer total %d", entry.PackID, record.EntryCount)
		}
		stored := selectedStored[entry.PackID]
		if entry.StoredLen > record.StoredBytes-stored {
			return restoredPackAuthority{}, fmt.Errorf("restored pack %s selected stored bytes exceed footer total %d", entry.PackID, record.StoredBytes)
		}
		selectedStored[entry.PackID] = stored + entry.StoredLen
	}
	for packID := range recordsByID {
		if selectedEntries[packID] == 0 {
			return restoredPackAuthority{}, fmt.Errorf("restored pack record %s has no selected mappings", packID)
		}
	}
	return authority, nil
}

func restoredAttachmentMembership(ctx context.Context, tx *sql.Tx) (map[packstore.Hash]struct{}, error) {
	rows, err := tx.QueryContext(ctx, attachmentReferencedHashesSQL)
	if err != nil {
		return nil, fmt.Errorf("list restored attachment membership: %w", err)
	}
	defer func() { _ = rows.Close() }()
	membership := make(map[packstore.Hash]struct{})
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan restored attachment membership: %w", err)
		}
		hash, err := packstore.ParseHash(raw)
		if err != nil {
			// Malformed legacy references remain untouched but cannot grant new
			// packed read authority.
			continue
		}
		membership[hash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate restored attachment membership: %w", err)
	}
	return membership, nil
}
