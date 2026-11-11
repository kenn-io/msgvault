package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
)

type xRestoreOptions struct {
	RecordPath string
	Head       string
	Isolated   bool
}

// Called under Kit's pinned target lease and, for configured archives, both
// daemon and direct-writer ownership. Snapshot contents cannot establish that
// overwriting the current target is safe: a pre-X snapshot has no X authority.
func refuseRestoreOverXArchive(ctx context.Context, root *os.Root) error {
	name := backupapp.New(Version).DBFileName()
	info, err := root.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing restore database: %w", err)
	}
	dbPath := filepath.Join(root.Name(), name)
	// SQLite takes a pathname. Verify it names the pinned directory/database
	// both before and after the read, so a renamed target cannot redirect it.
	verify := func() error {
		pinnedRoot, err := root.Stat(".")
		if err != nil {
			return err
		}
		namedRoot, err := os.Stat(root.Name())
		if err != nil {
			return err
		}
		pinnedDB, err := root.Stat(name)
		if err != nil {
			return err
		}
		namedDB, err := os.Stat(dbPath)
		if err != nil {
			return err
		}
		if !os.SameFile(pinnedRoot, namedRoot) || !os.SameFile(info, pinnedDB) || !os.SameFile(info, namedDB) {
			return errors.New("restore target changed during existing archive inspection")
		}
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	st, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("inspect existing archive before restore: %w", err)
	}
	hasX, inspectErr := st.HasXSourcesContext(ctx)
	if err := errors.Join(inspectErr, st.Close(), verify()); err != nil {
		return fmt.Errorf("inspect existing X authority before restore: %w", err)
	}
	if hasX {
		return errors.New("existing X archive requires a fresh isolated restore target; overwriting would discard current spending, receipts, and suppressions")
	}
	return nil
}

// prepareRestoredArchive operates only on Kit's unpublished private scratch
// DB. Freshness comes from an independently retained authenticated head, never
// from the snapshot or a hash supplied inside the record.
func prepareRestoredArchive(ctx context.Context, target backup.RestorePublicationTarget, opts xRestoreOptions) error {
	if err := replayRestoredXRemovals(ctx, target.DBPath, opts); err != nil {
		return err
	}
	// Seal the scratch database before Kit validates and publishes it.
	db, err := sql.Open(sqliteutil.DriverName(), target.DBPath)
	if err != nil {
		return fmt.Errorf("open restored database: %w", err)
	}
	db.SetMaxOpenConns(1)
	_, sealErr := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`)
	return errors.Join(sealErr, db.Close())
}

func replayRestoredXRemovals(ctx context.Context, dbPath string, opts xRestoreOptions) (retErr error) {
	readOnly, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return err
	}
	hasX, inspectErr := readOnly.HasXSourcesContext(ctx)
	var record store.XRemovalRecord
	if inspectErr == nil && (hasX || opts.RecordPath != "" || opts.Head != "") {
		switch {
		case !opts.Isolated:
			inspectErr = errors.New("x snapshot restore requires a fresh isolated target without overwrite or live-target coordination")
		case opts.RecordPath == "" || opts.Head == "":
			inspectErr = errors.New("x snapshot restore requires a complete current removal record and independently trusted current head before publication")
		default:
			record, inspectErr = readXRemovalRecord(opts.RecordPath)
			if inspectErr == nil {
				if !hasX && len(record.ProtectedAuthors) > 0 {
					inspectErr = errors.New("x author privacy cannot be retained without a restored X source")
				} else {
					inspectErr = readOnly.ValidateXRemovalRecordContext(ctx, record, opts.Head)
				}
			}
		}
	}
	if err := errors.Join(inspectErr, readOnly.Close()); err != nil {
		return err
	}
	if !hasX {
		for _, source := range record.Sources {
			if len(source.RemovedIDs) != 0 {
				return errors.New("x removal record contains suppressions but the restored snapshot has no X source to retain them")
			}
		}
		return nil
	}

	st, err := store.OpenContext(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, st.Close()) }()
	// Build scratch-only paths from a fresh config. A live config may carry
	// explicit analytics/vector paths outside the unpublished restore directory.
	scratch := config.NewDefaultConfig()
	scratch.HomeDir, scratch.Data.DataDir = filepath.Dir(dbPath), filepath.Dir(dbPath)
	scratch.Vector.DBPath = filepath.Join(filepath.Dir(dbPath), "vectors.db")
	cleaner := NewXContentCleanup(st, scratch, nil)
	sources, err := st.ListSourcesContext(ctx, "x")
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return errors.New("restored X source inventory changed")
	}
	present := make(map[int64]bool, len(sources))
	anchor := sources[0].ID
	for _, source := range sources {
		present[source.ID] = true
		anchor = min(anchor, source.ID)
	}
	if len(record.ProtectedAuthors) > 0 {
		execution, err := st.AcquireSyncExecutionContext(ctx, anchor)
		if err != nil {
			return err
		}
		for _, author := range record.ProtectedAuthors {
			if err := st.QueueXAuthorRemovalContext(ctx, anchor, author.UserID, author.ProtectedAt); err != nil {
				return errors.Join(err, execution.Release())
			}
		}
		if err := execution.Release(); err != nil {
			return err
		}
	}
	for _, source := range record.Sources {
		sourceID := source.SourceID
		if !present[sourceID] {
			// Native IDs are global across X accounts. Preserve even aliases
			// learned by a source created after this snapshot, without creating
			// that source or changing the snapshot's manifest populations.
			sourceID = anchor
		}
		for _, id := range source.RemovedIDs {
			if _, err := cleaner.RemovePost(ctx, sourceID, id, "removed"); err != nil {
				return fmt.Errorf("replay restored X removal: %w", err)
			}
		}
	}
	// Resume local work left pending in the snapshot without provider access.
	// Startup spill recovery belongs to the daemon and must not run here.
	if err := cleaner.Resume(ctx); err != nil {
		return err
	}
	pending, err := st.XCleanupPendingContext(ctx)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("restored X content cleanup is still pending")
	}
	return st.CheckpointWALContext(ctx)
}

func readXRemovalRecord(path string) (record store.XRemovalRecord, retErr error) {
	file, err := os.Open(path)
	if err != nil {
		return record, fmt.Errorf("open X removal record: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Size() > store.XMaxRemovalRecordBytes {
		return record, errors.New("x removal record must be a regular file no larger than 8 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, store.XMaxRemovalRecordBytes+1))
	if err != nil {
		return record, err
	}
	if len(data) > store.XMaxRemovalRecordBytes {
		return record, errors.New("x removal record exceeds 8 MiB")
	}
	if err := json.Unmarshal(data, &record, json.RejectUnknownMembers(true)); err != nil {
		return record, errors.New("malformed X removal record")
	}
	return record, nil
}
