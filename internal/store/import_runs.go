package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/sourcecatalog"
)

// ImportAttemptRequest is supplied by a native importer while it owns the
// existing source execution. Format identifies that parser, never a source label.
type ImportAttemptRequest struct {
	Principal          string
	SourceID           int64
	Format             string
	NotifyImports      bool
	SyncRunID          int64
	RecoveredSyncRunID int64
}

// ImportRun is a durable logical operation shared by recovered native attempts.
// SourceGeneration is internal authority and must not be returned by wire adapters.
type ImportRun struct {
	ID               string
	Principal        string
	SourceID         int64
	SourceGeneration string
	Format           string
	NotifyImports    bool
	completed        bool
}

func scanImportRun(row scanner) (ImportRun, error) {
	var run ImportRun
	err := row.Scan(&run.ID, &run.Principal, &run.SourceID, &run.SourceGeneration, &run.Format, &run.NotifyImports, &run.completed)
	return run, err
}

const importRunColumns = "id,principal,source_id,source_generation,import_format,notify_imports,completed"

func (s *Store) importAttemptRun(ctx context.Context, tx *sql.Tx, syncID int64) (ImportRun, error) {
	var id string
	// #nosec G701 -- SQL is fixed; Rebind changes placeholders and all values are bound.
	if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT import_run_id FROM import_run_attempts WHERE sync_run_id=?`), syncID).Scan(&id); err != nil {
		return ImportRun{}, err
	}
	// #nosec G701 -- SQL is fixed; Rebind changes placeholders and all values are bound.
	return scanImportRun(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+importRunColumns+` FROM import_runs WHERE id=?`), id))
}

func validateImportRun(run ImportRun, request ImportAttemptRequest, generation string) error {
	if run.completed || run.Principal != request.Principal || run.SourceID != request.SourceID || run.SourceGeneration != generation || run.Format != request.Format || run.NotifyImports != request.NotifyImports {
		return mcpStoreError("invalid_import_run")
	}
	return nil
}

func (s *Store) recoverImportRun(ctx context.Context, tx *sql.Tx, request ImportAttemptRequest, generation string) (ImportRun, error) {
	run, err := s.importAttemptRun(ctx, tx, request.RecoveredSyncRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return run, mcpStoreError("invalid_import_run")
	}
	if err != nil {
		return run, err
	}
	if err = validateImportRun(run, request, generation); err != nil {
		return run, err
	}
	var sourceID int64
	var status string
	var previousFingerprint, currentFingerprint sql.NullString
	if err = tx.QueryRowContext(ctx, s.Rebind(`SELECT source_id,status,request_fingerprint FROM sync_runs WHERE id=?`), request.RecoveredSyncRunID).Scan(&sourceID, &status, &previousFingerprint); err != nil {
		return run, err
	}
	if sourceID != request.SourceID || (status != SyncStatusRunning && status != SyncStatusFailed) {
		return run, mcpStoreError("invalid_import_run")
	}
	if err = tx.QueryRowContext(ctx, s.Rebind(`SELECT request_fingerprint FROM sync_runs WHERE id=?`), request.SyncRunID).Scan(&currentFingerprint); err != nil {
		return run, err
	}
	if previousFingerprint != currentFingerprint {
		return run, mcpStoreError("invalid_import_run")
	}
	return run, nil
}

// BeginImportAttempt records policy before the first message or checkpoint.
// It reuses identity -> native sync generation -> Events clock ordering and
// never takes a second source execution lease or performs provider/file IO.
func (s *Store) BeginImportAttempt(ctx context.Context, request ImportAttemptRequest) (ImportRun, error) {
	var result ImportRun
	descriptor, known := sourcecatalog.Lookup(request.Format)
	if !known || !descriptor.Imports {
		return result, mcpStoreError("invalid_import_format")
	}
	if request.SourceID <= 0 || request.SyncRunID <= 0 || request.RecoveredSyncRunID < 0 || request.RecoveredSyncRunID == request.SyncRunID || s.syncGeneration == nil || s.syncGeneration.sourceID != request.SourceID || s.syncGeneration.runID != request.SyncRunID {
		return result, mcpStoreError("invalid_import_run")
	}
	root := s.mcpRoot()
	root.mcpConfigMu.Lock()
	defer root.mcpConfigMu.Unlock()
	runtime := root.mcpConfig.Load()
	owner := ""
	if runtime != nil {
		owner = runtime.config.Principal
	}
	if request.Principal != owner || request.NotifyImports && owner == "" {
		return result, mcpStoreError("owner_required")
	}
	if err := validateNativeImportChoice(runtime, request.Format, request.NotifyImports); err != nil {
		return result, err
	}
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, "source", request.SourceID, request.SourceID)
		if err != nil {
			return err
		}
		existing, err := s.importAttemptRun(ctx, tx, request.SyncRunID)
		if err == nil {
			if err = validateImportRun(existing, request, lifetime.source); err != nil {
				return err
			}
			if request.RecoveredSyncRunID != 0 {
				recovered, err := s.recoverImportRun(ctx, tx, request, lifetime.source)
				if err != nil {
					return err
				}
				if recovered.ID != existing.ID {
					return mcpStoreError("invalid_import_run")
				}
			}
			result = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if request.RecoveredSyncRunID != 0 {
			result, err = s.recoverImportRun(ctx, tx, request, lifetime.source)
			if err != nil {
				return err
			}
		} else {
			var random [16]byte
			if _, err = rand.Read(random[:]); err != nil {
				return fmt.Errorf("generate import run identity: %w", err)
			}
			result = ImportRun{ID: "import_" + hex.EncodeToString(random[:]), Principal: request.Principal, SourceID: request.SourceID, SourceGeneration: lifetime.source, Format: request.Format, NotifyImports: request.NotifyImports}
			if _, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO import_runs (id,principal,source_id,source_generation,import_format,notify_imports,created_at) VALUES (?,?,?,?,?,?,?)`), result.ID, result.Principal, result.SourceID, result.SourceGeneration, result.Format, result.NotifyImports, mcpTime(time.Now().UTC())); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO import_run_attempts (sync_run_id,import_run_id) VALUES (?,?)`), request.SyncRunID, result.ID)
		return err
	})
	if err != nil {
		return ImportRun{}, err
	}
	return result, nil
}

// completeImportRunTx commits logical completion with the native terminal
// transition. Attempt retention must never reopen a completed logical run.
func (s *Store) completeImportRunTx(ctx context.Context, tx *sql.Tx, syncID int64) error {
	_, err := tx.ExecContext(ctx, s.Rebind(`UPDATE import_runs SET completed=TRUE WHERE NOT completed AND EXISTS (SELECT 1 FROM import_run_attempts a WHERE a.import_run_id=import_runs.id AND a.sync_run_id=?)`), syncID)
	return err
}

// BeginNativeImportAttempt derives owner identity from the daemon's validated
// Store configuration. Native dispatch supplies parser identity and the source
// generation; neither owner nor format is copied from an external request.
func (s *Store) BeginNativeImportAttempt(ctx context.Context, sourceID int64, format string, notify bool, syncID, recoveredSyncID int64) (ImportRun, error) {
	principal := ""
	if runtime := s.mcpRoot().mcpConfig.Load(); runtime != nil {
		principal = runtime.config.Principal
	}
	return s.BeginImportAttempt(ctx, ImportAttemptRequest{Principal: principal, SourceID: sourceID, Format: format, NotifyImports: notify, SyncRunID: syncID, RecoveredSyncRunID: recoveredSyncID})
}

// FindResumableImportAttempt reads the durable attempt mapping while the native
// importer owns source execution. Progress checkpoints are not policy evidence.
// A completed fresh run supersedes older abandoned attempts of this format.
func (s *Store) FindResumableImportAttempt(ctx context.Context, sourceID int64, format, requestFingerprint string) (int64, error) {
	var syncID int64
	var previous sql.NullString
	err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT a.sync_run_id,s.request_fingerprint
 FROM import_run_attempts a
 JOIN import_runs r ON r.id=a.import_run_id
 JOIN sync_runs s ON s.id=a.sync_run_id
 WHERE r.source_id=? AND r.import_format=? AND NOT r.completed
   AND s.source_id=r.source_id AND s.status IN ('running','failed')
   AND a.sync_run_id>COALESCE((
     SELECT MAX(done.sync_run_id)
     FROM import_run_attempts done
     JOIN import_runs completed ON completed.id=done.import_run_id
     WHERE completed.source_id=r.source_id
       AND completed.import_format=r.import_format AND completed.completed
   ),0)
 ORDER BY a.sync_run_id DESC LIMIT 1`), sourceID, format).Scan(&syncID, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, mcpSafeError(err)
	}
	if !previous.Valid || previous.String != requestFingerprint {
		return 0, mcpStoreError("invalid_import_run")
	}
	return syncID, nil
}

// WithImportRun constructs an immutable view only for the current native
// execution attempt. An opaque run ID is not authority to ingest messages.
func (s *Store) WithImportRun(ctx context.Context, runID string) (*Store, error) {
	if runID == "" || s.syncGeneration == nil || s.syncGeneration.sourceID <= 0 || s.syncGeneration.runID <= 0 {
		return nil, mcpStoreError("invalid_import_run")
	}
	root := s.mcpRoot()
	root.mcpConfigMu.Lock()
	defer root.mcpConfigMu.Unlock()
	var run ImportRun
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		current, err := s.importAttemptRun(ctx, tx, s.syncGeneration.runID)
		if errors.Is(err, sql.ErrNoRows) {
			return mcpStoreError("invalid_import_run")
		}
		if err != nil {
			return err
		}
		if current.ID != runID || current.completed || current.SourceID != s.syncGeneration.sourceID {
			return mcpStoreError("invalid_import_run")
		}
		lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, "source", current.SourceID, current.SourceID)
		if err != nil {
			return err
		}
		if current.SourceGeneration != lifetime.source {
			return mcpStoreError("invalid_import_run")
		}
		runtime := root.mcpConfig.Load()
		principal := ""
		if runtime != nil {
			principal = runtime.config.Principal
		}
		if current.Principal != principal {
			return mcpStoreError("owner_required")
		}
		if err := validateNativeImportChoice(runtime, current.Format, current.NotifyImports); err != nil {
			return err
		}
		run = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.WithIngestContext(IngestContext{Mode: IngestImport, ObservedAt: s.mcpIngest.ObservedAt, importRun: &run}), nil
}

// validateImportWriteTx rechecks durable and current authority under the
// canonical writer fences. Construction alone cannot authorize later writes.
func (s *Store) validateImportWriteTx(ctx context.Context, tx *loggedTx) error {
	if s.mcpIngest.Mode != IngestImport {
		return nil
	}
	expected := s.mcpIngest.importRun
	if expected == nil || s.syncGeneration == nil || expected.SourceID != s.syncGeneration.sourceID {
		return mcpStoreError("invalid_import_run")
	}
	current, err := s.importAttemptRun(ctx, tx.Tx, s.syncGeneration.runID)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpStoreError("invalid_import_run")
	}
	if err != nil {
		return mcpSafeError(err)
	}
	if current.ID != expected.ID || current.completed || current.Principal != expected.Principal || current.SourceID != expected.SourceID || current.SourceGeneration != expected.SourceGeneration || current.Format != expected.Format || current.NotifyImports != expected.NotifyImports {
		return mcpStoreError("invalid_import_run")
	}
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx.Tx, "source", current.SourceID, current.SourceID)
	if err != nil {
		return mcpSafeError(err)
	}
	if lifetime.source != current.SourceGeneration {
		return mcpStoreError("invalid_import_run")
	}
	runtime := s.mcpRoot().mcpConfig.Load()
	principal := ""
	if runtime != nil {
		principal = runtime.config.Principal
	}
	if principal != current.Principal {
		return mcpStoreError("owner_required")
	}
	if err := validateNativeImportChoice(runtime, current.Format, current.NotifyImports); err != nil {
		return err
	}
	return nil
}

// ValidateNativeImportChoice rejects an unavailable explicit notification request
// before native dispatch creates sources or progress records. Format identifies
// the native parser; custom archive source labels never select this authority.
func (s *Store) ValidateNativeImportChoice(ctx context.Context, format string, notify bool) error {
	// Silent imports retain the native resumable cancellation lifecycle.
	if !notify {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root := s.mcpRoot()
	root.mcpConfigMu.Lock()
	defer root.mcpConfigMu.Unlock()
	return validateNativeImportChoice(root.mcpConfig.Load(), format, notify)
}

// Callers recheck under the configuration and writer fences so producer loss
// cannot turn a previously admitted notifying run into a silent archive write.
func validateNativeImportChoice(runtime *mcpRuntime, format string, notify bool) error {
	if !notify {
		return nil
	}
	if runtime == nil || !runtime.config.Enabled || !runtime.config.ImportNotifications {
		return mcpStoreError("import_notifications_disabled")
	}
	if runtime.config.Principal == "" {
		return mcpStoreError("owner_required")
	}
	if !runtime.kinds["msgvault.import_message_archived\x00"+format]["message"] {
		return mcpStoreError("import_notifications_unavailable")
	}
	return nil
}
