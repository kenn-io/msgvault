package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.TriageMappingStore = (*Store)(nil)

// InboxTriageMappings reads exact-source configuration in one archive snapshot.
// Revision zero means it has never been configured; clearing retains a revision.
func (s *Store) InboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity) (map[string]string, int64, error) {
	if err := source.Validate(); err != nil {
		return nil, 0, err
	}
	entries := map[string]string{}
	var revision int64
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxCandidateSource(ctx, tx, source); err != nil {
			return err
		}
		var err error
		entries, revision, err = readInboxTriageMappings(ctx, tx, source)
		return err
	})
	if err != nil {
		return nil, revision, err
	}
	return entries, revision, nil
}

func readInboxTriageMappings(ctx context.Context, tx *loggedTx, source inboxcontrol.SourceIdentity) (map[string]string, int64, error) {
	entries := map[string]string{}
	var revision int64
	var sourceJSON, mappingJSON string
	err := tx.QueryRowContext(ctx, `SELECT source_json,mapping_json,revision FROM inbox_triage_mappings WHERE source_id=?`, source.SourceID).Scan(&sourceJSON, &mappingJSON, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return entries, 0, nil
	}
	if err != nil {
		return nil, revision, fmt.Errorf("read inbox triage mappings: %w", err)
	}
	var bound inboxcontrol.SourceIdentity
	if json.Unmarshal([]byte(sourceJSON), &bound, json.RejectUnknownMembers(true)) != nil || bound.Validate() != nil || revision <= 0 {
		return nil, revision, inboxcontrol.ErrUnavailable
	}
	if bound != source {
		return nil, revision, inboxcontrol.ErrPlanChanged
	}
	if json.Unmarshal([]byte(mappingJSON), &entries) != nil || entries == nil || inboxcontrol.ValidateTriageMappingEntries(source, entries) != nil {
		return nil, revision, inboxcontrol.ErrUnavailable
	}
	return entries, revision, nil
}

// ReplaceInboxTriageMappings persists owner configuration already validated
// against the native catalog by Service.UpdateTriageMappings. That service
// holds the write gate and source lease. Store rechecks source and owner shape
// and atomically compares the durable revision; this never edits provider state.
func (s *Store) ReplaceInboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expectedRevision int64, principal inboxcontrol.Principal) (int64, error) {
	if err := inboxcontrol.ValidateTriageMappingUpdate(source, entries, expectedRevision, principal); err != nil {
		return 0, err
	}
	if entries == nil {
		entries = map[string]string{}
	}
	bound, err := json.Marshal(source)
	if err != nil {
		return 0, fmt.Errorf("encode inbox triage source: %w", err)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return 0, fmt.Errorf("encode inbox triage mappings: %w", err)
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxCandidateSource(ctx, tx, source); err != nil {
			return err
		}
		var result sql.Result
		var err error
		if expectedRevision == 0 {
			result, err = tx.ExecContext(ctx, `INSERT INTO inbox_triage_mappings(source_id,source_json,mapping_json,revision)
 VALUES (?,?,?,1) ON CONFLICT(source_id) DO NOTHING`, source.SourceID, string(bound), string(encoded))
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE inbox_triage_mappings SET source_json=?,mapping_json=?,revision=revision+1
 WHERE source_id=? AND revision=?`, string(bound), string(encoded), source.SourceID, expectedRevision)
		}
		if err != nil {
			return fmt.Errorf("replace inbox triage mappings: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count inbox triage mapping updates: %w", err)
		}
		if changed != 1 {
			return inboxcontrol.ErrConflict
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expectedRevision + 1, nil
}
