package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/filenamediscovery"
)

const filenameDiscoveryConsumer = "filename-discovery/v1"
const filenameDiscoveryBatchSize = 500
const filenameDiscoveryStateColumns = `generation, revision, scan_after, scan_end,
	baseline, catchup_fence, last_sequence, skipped, phase, gc_turn`

// FilenameDiscoveryStatus is maintenance-only. Request readers use the ready
// generation on their authority snapshot and never initialize it themselves.
type FilenameDiscoveryStatus struct {
	Generation, Revision, ScanAfter, ScanEnd      int64
	Baseline, CatchupFence, LastSequence, Skipped int64
	Phase                                         string
}

type FilenameDiscoveryBatch struct {
	Processed int
	More      bool
	Status    FilenameDiscoveryStatus
}

type filenameDiscoveryState struct {
	FilenameDiscoveryStatus

	gcTurn bool
}

type filenameProjectionUpdate struct {
	id           int64
	sequence     int64
	currentName  string
	currentFound bool
}

type filenameProjectionPrevious struct {
	sequence int64
	skipped  bool
}

type filenameProjectionEntry struct {
	id    int64
	name  filenamediscovery.Name
	hash  string
	valid bool
}

func scanFilenameDiscoveryState(row *sql.Row) (filenameDiscoveryState, error) {
	var state filenameDiscoveryState
	err := row.Scan(&state.Generation, &state.Revision, &state.ScanAfter, &state.ScanEnd,
		&state.Baseline, &state.CatchupFence, &state.LastSequence, &state.Skipped, &state.Phase, &state.gcTurn)
	return state, err
}

func (s *Store) FilenameDiscoveryStatusContext(ctx context.Context) (FilenameDiscoveryStatus, error) {
	state, err := scanFilenameDiscoveryState(ReadDBContext(ctx, s.db.DB).QueryRowContext(ctx,
		`SELECT `+filenameDiscoveryStateColumns+` FROM filename_discovery_state WHERE singleton=1`))
	if err != nil {
		return FilenameDiscoveryStatus{}, fmt.Errorf("read filename discovery status: %w", err)
	}
	return state.FilenameDiscoveryStatus, nil
}

// filenameDiscoveryTransaction takes the archive writer boundary before any
// read, including the Store sync-generation fence. READ COMMITTED is essential
// on PostgreSQL: a snapshot from before the fence can miss a late commit.
func (s *Store) filenameDiscoveryTransaction(ctx context.Context, fn func(context.Context, *loggedTx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var opts *sql.TxOptions
	if s.IsPostgreSQL() {
		opts = &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	}
	return s.withTxLockedContext(ctx, opts, func(tx *loggedTx) error {
		if s.IsPostgreSQL() {
			if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
				return fmt.Errorf("set filename discovery lock timeout: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `LOCK TABLE attachments, messages IN SHARE MODE`); err != nil {
				return fmt.Errorf("lock filename discovery writer boundary: %w", err)
			}
			return nil
		}
		// INSERT reserves SQLite's writer slot even when the singleton exists.
		_, err := tx.ExecContext(ctx, `INSERT INTO filename_discovery_state(singleton)
			VALUES(1) ON CONFLICT(singleton) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("reserve filename discovery writer: %w", err)
		}
		return nil
	}, func(tx *loggedTx) error { return fn(ctx, tx) })
}

func (s *Store) filenameDiscoveryStateTx(ctx context.Context, tx *loggedTx) (filenameDiscoveryState, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO filename_discovery_state(singleton)
		VALUES(1) ON CONFLICT(singleton) DO NOTHING`); err != nil {
		return filenameDiscoveryState{}, err
	}
	query := `SELECT ` + filenameDiscoveryStateColumns + ` FROM filename_discovery_state WHERE singleton=1`
	if s.IsPostgreSQL() {
		query += s.dialect.SelectForUpdate()
	}
	return scanFilenameDiscoveryState(tx.QueryRowContext(ctx, query))
}

func saveFilenameDiscoveryState(ctx context.Context, tx *loggedTx, state filenameDiscoveryState) error {
	_, err := tx.ExecContext(ctx, `UPDATE filename_discovery_state SET generation=?, revision=?,
		scan_after=?, scan_end=?, baseline=?, catchup_fence=?, last_sequence=?, skipped=?, phase=?, gc_turn=?
		WHERE singleton=1`, state.Generation, state.Revision, state.ScanAfter, state.ScanEnd,
		state.Baseline, state.CatchupFence, state.LastSequence, state.Skipped, state.Phase, state.gcTurn)
	return err
}

// startFilenameDiscoveryTx resets only our consumer at the safe writer boundary.
// Existing generations remain for bounded cleanup after the new one is ready.
func (s *Store) startFilenameDiscoveryTx(ctx context.Context, tx *loggedTx, state *filenameDiscoveryState) error {
	q := boundQuerier{ctx: ctx, q: tx}
	baseline, err := attachmentChangeBaseline(q)
	if err != nil {
		return err
	}
	var maximum, oldGeneration int64
	if err := q.QueryRow(`SELECT COALESCE(MAX(id),0) FROM attachments`).Scan(&maximum); err != nil {
		return err
	}
	if err := q.QueryRow(`SELECT COALESCE(MAX(generation),0) FROM filename_discovery_entries`).Scan(&oldGeneration); err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT INTO attachment_change_consumers
		(consumer_key, baseline_sequence, last_sequence, reconciliation_complete)
		VALUES (?, ?, ?, FALSE) ON CONFLICT(consumer_key) DO UPDATE SET
		baseline_sequence=excluded.baseline_sequence, last_sequence=excluded.last_sequence,
		reconciliation_complete=FALSE, updated_at=`+s.dialect.Now(), filenameDiscoveryConsumer, baseline, baseline); err != nil {
		return err
	}
	*state = filenameDiscoveryState{
		Generation: max(state.Generation, oldGeneration) + 1, Revision: state.Revision + 1,
		ScanEnd: maximum, Baseline: baseline, LastSequence: baseline, Phase: "bootstrap",
	}
	return nil
}

func (s *Store) ResetFilenameDiscoveryContext(ctx context.Context) error {
	return s.filenameDiscoveryTransaction(ctx, func(ctx context.Context, tx *loggedTx) error {
		state, err := s.filenameDiscoveryStateTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.startFilenameDiscoveryTx(ctx, tx, &state); err != nil {
			return err
		}
		return saveFilenameDiscoveryState(ctx, tx, state)
	})
}

// FilenameDiscoveryBatchContext commits at most one source, replay, or cleanup
// page. More asks the scheduler for another pass; it never drains here.
func (s *Store) FilenameDiscoveryBatchContext(ctx context.Context) (FilenameDiscoveryBatch, error) {
	if status, idle, err := s.filenameDiscoveryIdleContext(ctx); err != nil {
		return FilenameDiscoveryBatch{}, err
	} else if idle {
		return FilenameDiscoveryBatch{Status: status}, nil
	}
	var batch FilenameDiscoveryBatch
	err := s.filenameDiscoveryTransaction(ctx, func(ctx context.Context, tx *loggedTx) error {
		state, err := s.filenameDiscoveryStateTx(ctx, tx)
		if err != nil {
			return err
		}
		q := boundQuerier{ctx: ctx, q: tx}
		var consumer AttachmentChangeConsumer
		err = scanAttachmentChangeConsumer(q, filenameDiscoveryConsumer, &consumer)
		if err != nil && !errors.Is(err, ErrAttachmentChangeConsumerMissing) {
			return err
		}
		if state.Generation == 0 || errors.Is(err, ErrAttachmentChangeConsumerMissing) ||
			consumer.BaselineSequence != state.Baseline || consumer.LastSequence != state.LastSequence ||
			consumer.ReconciliationComplete != (state.Phase != "bootstrap") {
			if err := s.startFilenameDiscoveryTx(ctx, tx, &state); err != nil {
				return err
			}
		}
		switch state.Phase {
		case "bootstrap":
			batch.Processed, err = s.filenameBootstrapTx(ctx, tx, &state)
			batch.More = true // catchup always has its own invocation, even when empty.
		case "catchup":
			batch.Processed, batch.More, err = s.filenameReplayTx(ctx, tx, &state, state.CatchupFence)
			if err == nil && state.Phase == "ready" {
				// Readiness has a fixed fence, while later events and old
				// generations still request a separate bounded scheduler pass.
				err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM filename_discovery_entries WHERE generation<?)
					OR EXISTS(SELECT 1 FROM attachment_change_log WHERE sequence>?)`,
					state.Generation, state.LastSequence).Scan(&batch.More)
			}
		case "ready":
			var old bool
			err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM filename_discovery_entries WHERE generation<?)`, state.Generation).Scan(&old)
			if err != nil {
				return err
			}
			high, highErr := attachmentChangeBaseline(q)
			if highErr != nil {
				return highErr
			}
			if old && (state.gcTurn || high <= state.LastSequence) {
				batch.Processed, err = filenameCleanupTx(ctx, tx, state.Generation)
				state.gcTurn = false
				batch.More = batch.Processed == filenameDiscoveryBatchSize || high > state.LastSequence
				if batch.Processed > 0 {
					state.Revision++
				}
			} else {
				batch.Processed, batch.More, err = s.filenameReplayTx(ctx, tx, &state, high)
				if batch.Processed > 0 {
					state.gcTurn = true
				}
				batch.More = batch.More || old
			}
		}
		if err != nil {
			return fmt.Errorf("maintain filename discovery: %w", err)
		}
		if err := saveFilenameDiscoveryState(ctx, tx, state); err != nil {
			return fmt.Errorf("save filename discovery progress: %w", err)
		}
		batch.Status = state.FilenameDiscoveryStatus
		return nil
	})
	if err != nil {
		return FilenameDiscoveryBatch{}, err
	}
	return batch, nil
}

// filenameDiscoveryIdleContext reports, without a write transaction, that a
// ready index has no journal events or old generations left to process. An
// idle daemon then takes no writer slot or PostgreSQL table lock every pass.
// A PostgreSQL event committed after this read is processed by the next pass.
func (s *Store) filenameDiscoveryIdleContext(ctx context.Context) (FilenameDiscoveryStatus, bool, error) {
	var state filenameDiscoveryState
	var idle bool
	err := ReadDBContext(ctx, s.db.DB).QueryRowContext(ctx, s.Rebind(`SELECT `+filenameDiscoveryStateColumns+`,
		st.generation>0 AND st.phase='ready' AND EXISTS (
			SELECT 1 FROM attachment_change_consumers c WHERE c.consumer_key=?
			AND c.reconciliation_complete=TRUE AND c.baseline_sequence=st.baseline
			AND c.last_sequence=st.last_sequence)
		AND NOT EXISTS (SELECT 1 FROM filename_discovery_entries e WHERE e.generation<st.generation)
		AND NOT EXISTS (SELECT 1 FROM attachment_change_log l WHERE l.sequence>st.last_sequence)
		FROM filename_discovery_state st WHERE st.singleton=1`), filenameDiscoveryConsumer).Scan(
		&state.Generation, &state.Revision, &state.ScanAfter, &state.ScanEnd, &state.Baseline,
		&state.CatchupFence, &state.LastSequence, &state.Skipped, &state.Phase, &state.gcTurn, &idle)
	if errors.Is(err, sql.ErrNoRows) {
		return FilenameDiscoveryStatus{}, false, nil
	}
	if err != nil {
		return FilenameDiscoveryStatus{}, false, fmt.Errorf("check filename discovery work: %w", err)
	}
	return state.FilenameDiscoveryStatus, idle, nil
}

func (s *Store) filenameBootstrapTx(ctx context.Context, tx *loggedTx, state *filenameDiscoveryState) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(filename,'') FROM attachments
		WHERE id>? AND id<=? ORDER BY id LIMIT ?`, state.ScanAfter, state.ScanEnd, filenameDiscoveryBatchSize)
	if err != nil {
		return 0, err
	}
	type entry struct {
		id   int64
		name string
	}
	entries := make([]entry, 0, filenameDiscoveryBatchSize)
	for rows.Next() {
		var value entry
		if err := rows.Scan(&value.id, &value.name); err != nil {
			_ = rows.Close()
			return 0, err
		}
		entries = append(entries, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	updates := make([]filenameProjectionUpdate, 0, len(entries))
	for _, value := range entries {
		updates = append(updates, filenameProjectionUpdate{
			id: value.id, sequence: state.Baseline, currentName: value.name, currentFound: true,
		})
	}
	if err := s.filenameApplyBatchTx(ctx, tx, state, updates); err != nil {
		return 0, err
	}
	if len(entries) > 0 {
		state.ScanAfter = entries[len(entries)-1].id
		state.Revision++
	}
	if len(entries) < filenameDiscoveryBatchSize {
		fence, err := attachmentChangeBaseline(boundQuerier{ctx: ctx, q: tx})
		if err != nil {
			return 0, err
		}
		state.CatchupFence, state.Phase = fence, "catchup"
		state.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE attachment_change_consumers SET reconciliation_complete=TRUE,
			updated_at=`+s.dialect.Now()+` WHERE consumer_key=?`, filenameDiscoveryConsumer); err != nil {
			return 0, err
		}
	}
	return len(entries), nil
}

func (s *Store) filenameReplayTx(ctx context.Context, tx *loggedTx, state *filenameDiscoveryState, fence int64) (int, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence, old_attachment_id, new_attachment_id FROM attachment_change_log
		WHERE sequence>? AND sequence<=? ORDER BY sequence LIMIT ?`, state.LastSequence, fence, filenameDiscoveryBatchSize)
	if err != nil {
		return 0, false, err
	}
	ids := make(map[int64]int64)
	processed, last := 0, state.LastSequence
	for rows.Next() {
		var sequence int64
		var oldID, newID sql.NullInt64
		if err := rows.Scan(&sequence, &oldID, &newID); err != nil {
			_ = rows.Close()
			return 0, false, err
		}
		processed++
		last = sequence
		for _, id := range []sql.NullInt64{oldID, newID} {
			if id.Valid {
				ids[id.Int64] = max(ids[id.Int64], sequence)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, false, err
	}
	if err := rows.Close(); err != nil {
		return 0, false, err
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	current, err := filenameCurrentAttachmentsTx(ctx, tx, ordered)
	if err != nil {
		return 0, false, err
	}
	updates := make([]filenameProjectionUpdate, 0, len(ordered))
	for _, id := range ordered {
		name, found := current[id]
		updates = append(updates, filenameProjectionUpdate{
			id: id, sequence: ids[id], currentName: name, currentFound: found,
		})
	}
	if err := s.filenameApplyBatchTx(ctx, tx, state, updates); err != nil {
		return 0, false, err
	}
	exhausted := processed < filenameDiscoveryBatchSize || last >= fence
	if exhausted {
		last = fence
	}
	changed := last != state.LastSequence
	state.LastSequence = last
	if state.Phase == "catchup" && exhausted {
		state.Phase = "ready"
		state.gcTurn = true
		changed = true
	}
	if changed || processed > 0 {
		// Apply, acknowledgement, durable high-water and cross-consumer pruning
		// share this transaction. A failing acknowledgement rolls them all back.
		if _, err := tx.ExecContext(ctx, `UPDATE attachment_change_consumers SET last_sequence=?,
			updated_at=`+s.dialect.Now()+` WHERE consumer_key=?`, last, filenameDiscoveryConsumer); err != nil {
			return 0, false, err
		}
		if err := pruneAttachmentChanges(boundQuerier{ctx: ctx, q: tx}); err != nil {
			return 0, false, err
		}
		state.Revision++
	}
	return processed, !exhausted, nil
}

func (s *Store) filenameApplyBatchTx(ctx context.Context, tx *loggedTx, state *filenameDiscoveryState, updates []filenameProjectionUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(updates))
	for _, update := range updates {
		ids = append(ids, update.id)
	}
	previous, err := filenamePreviousEntriesTx(ctx, tx, state.Generation, ids)
	if err != nil {
		return err
	}

	toDelete := make([]int64, 0, len(updates))
	entries := make([]filenameProjectionEntry, 0, len(updates))
	entryRows := make([][]any, 0, len(updates))
	for _, update := range updates {
		if err := ctx.Err(); err != nil {
			return err
		}
		old, exists := previous[update.id]
		if exists && old.sequence > update.sequence {
			continue
		}
		if exists {
			toDelete = append(toDelete, update.id)
			if old.skipped {
				state.Skipped--
			}
		}
		if !update.currentFound {
			continue
		}

		name, valid := filenamediscovery.ProjectName(update.currentName)
		if !valid {
			state.Skipped++
		}
		hash := ""
		if valid {
			sum := sha256.Sum256([]byte(name.Normalized))
			hash = hex.EncodeToString(sum[:])
		}
		entry := filenameProjectionEntry{
			id: update.id, name: name, hash: hash, valid: valid,
		}
		entries = append(entries, entry)
		entryRows = append(entryRows, []any{
			state.Generation, update.id, name.Normalized, hash, update.sequence, !valid,
		})
	}
	if err := filenameDeleteEntriesTx(ctx, tx, state.Generation, toDelete); err != nil {
		return err
	}
	if err := filenameInsertRowsTx(ctx, tx, "filename_discovery_entries",
		"generation,attachment_id,normalized_name,name_hash,applied_sequence,skipped", entryRows); err != nil {
		return err
	}

	tokenRows := make([][]any, 0, 128)
	deleteRows := make([][]any, 0, 128)
	flushTokens := func() error {
		if len(tokenRows) == 0 {
			return nil
		}
		if err := filenameInsertRowsTx(ctx, tx, "filename_discovery_tokens",
			"generation,attachment_id,token_key", tokenRows); err != nil {
			return err
		}
		tokenRows = tokenRows[:0]
		return nil
	}
	flushDeletes := func() error {
		if len(deleteRows) == 0 {
			return nil
		}
		if err := filenameInsertRowsTx(ctx, tx, "filename_discovery_deletes",
			"generation,attachment_id,delete_key,token_key", deleteRows); err != nil {
			return err
		}
		deleteRows = deleteRows[:0]
		return nil
	}
	for _, entry := range entries {
		if !entry.valid {
			continue
		}
		for _, token := range entry.name.Tokens {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := hex.EncodeToString([]byte(token))
			tokenRows = append(tokenRows, []any{state.Generation, entry.id, key})
			if len(tokenRows) == cap(tokenRows) {
				if err := flushTokens(); err != nil {
					return err
				}
			}
			if utf8.RuneCountInString(token) < 4 {
				continue
			}
			for _, deletion := range filenamediscovery.DeleteKeys(token) {
				deleteRows = append(deleteRows, []any{
					state.Generation, entry.id, hex.EncodeToString([]byte(deletion)), key,
				})
				if len(deleteRows) == cap(deleteRows) {
					if err := flushDeletes(); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := flushTokens(); err != nil {
		return err
	}
	return flushDeletes()
}

func filenamePreviousEntriesTx(ctx context.Context, tx *loggedTx, generation int64, ids []int64) (map[int64]filenameProjectionPrevious, error) {
	previous := make(map[int64]filenameProjectionPrevious, len(ids))
	for len(ids) > 0 {
		page := ids[:min(len(ids), 128)]
		query := `SELECT attachment_id, applied_sequence, skipped FROM filename_discovery_entries
			WHERE generation=? AND attachment_id IN (` + filenameIDPlaceholders(len(page)) + `)`
		args := make([]any, 1, len(page)+1)
		args[0] = generation
		for _, id := range page {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var value filenameProjectionPrevious
			if err := rows.Scan(&id, &value.sequence, &value.skipped); err != nil {
				_ = rows.Close()
				return nil, err
			}
			previous[id] = value
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		ids = ids[len(page):]
	}
	return previous, nil
}

func filenameCurrentAttachmentsTx(ctx context.Context, tx *loggedTx, ids []int64) (map[int64]string, error) {
	current := make(map[int64]string, len(ids))
	for len(ids) > 0 {
		page := ids[:min(len(ids), 128)]
		query := `SELECT id, COALESCE(filename,'') FROM attachments WHERE id IN (` + filenameIDPlaceholders(len(page)) + `)`
		args := make([]any, 0, len(page))
		for _, id := range page {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				_ = rows.Close()
				return nil, err
			}
			current[id] = name
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		ids = ids[len(page):]
	}
	return current, nil
}

func filenameDeleteEntriesTx(ctx context.Context, tx *loggedTx, generation int64, ids []int64) error {
	for len(ids) > 0 {
		page := ids[:min(len(ids), 128)]
		query := `DELETE FROM filename_discovery_entries WHERE generation=? AND attachment_id IN (` + filenameIDPlaceholders(len(page)) + `)`
		args := make([]any, 1, len(page)+1)
		args[0] = generation
		for _, id := range page {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		ids = ids[len(page):]
	}
	return nil
}

func filenameInsertRowsTx(ctx context.Context, tx *loggedTx, table, columns string, rows [][]any) error {
	for len(rows) > 0 {
		page := rows[:min(len(rows), 128)]
		placeholders := "(" + strings.TrimSuffix(strings.Repeat("?,", len(page[0])), ",") + ")"
		values := make([]string, len(page))
		args := make([]any, 0, len(page)*len(page[0]))
		for i, row := range page {
			values[i] = placeholders
			args = append(args, row...)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+` (`+columns+`) VALUES `+strings.Join(values, ","), args...); err != nil {
			return err
		}
		rows = rows[len(page):]
	}
	return nil
}

func filenameIDPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func filenameCleanupTx(ctx context.Context, tx *loggedTx, generation int64) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT generation,attachment_id FROM filename_discovery_entries
		WHERE generation<? ORDER BY generation,attachment_id LIMIT ?`, generation, filenameDiscoveryBatchSize)
	if err != nil {
		return 0, err
	}
	type key struct{ generation, id int64 }
	keys := make([]key, 0, filenameDiscoveryBatchSize)
	for rows.Next() {
		var value key
		if err := rows.Scan(&value.generation, &value.id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		keys = append(keys, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	byGeneration := make(map[int64][]int64)
	for _, key := range keys {
		byGeneration[key.generation] = append(byGeneration[key.generation], key.id)
	}
	for generation, ids := range byGeneration {
		if err := filenameDeleteEntriesTx(ctx, tx, generation, ids); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}
