package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"sort"
	"strings"
)

const XMaxRemovalRecordBytes = 8 << 20

// XRemovalRecord binds permanent suppressions and a content-free fingerprint
// of spending and send authority to the current archive before restoration.
type XRemovalRecord struct {
	Version    int                    `json:"version"`
	ArchiveUID string                 `json:"archive_uid"`
	Sources    []XRemovalRecordSource `json:"sources"`
}

type XRemovalRecordSource struct {
	SourceID      int64    `json:"source_id"`
	OwnerID       string   `json:"owner_id"`
	InstanceID    string   `json:"instance_id"`
	FinancialHead string   `json:"financial_head"`
	TargetHead    string   `json:"target_head"`
	RemovedIDs    []string `json:"removed_ids"`
}

// ExportXRemovalRecordContext captures all current X sources, suppression
// aliases and financial fingerprints in one read snapshot. Its head must be retained independently of any
// backup or supplied record; a record cannot attest to its own freshness.
func (s *Store) ExportXRemovalRecordContext(ctx context.Context) (record XRemovalRecord, head string, retErr error) {
	if s.syncGeneration != nil {
		return record, "", errors.New("X removal authority requires the owner Store")
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		record, err = s.xRemovalRecordTx(ctx, tx, true)
		return err
	})
	if err != nil {
		return XRemovalRecord{}, "", err
	}
	normalized, data, err := normalizeXRemovalRecord(record)
	if err != nil {
		return XRemovalRecord{}, "", err
	}
	digest := sha256.Sum256(data)
	return normalized, hex.EncodeToString(digest[:]), nil
}

// CaptureXRemovalRecordContext fences the complete X source inventory while
// exporting. Backend source keys share one session so even a small PostgreSQL
// pool retains a connection for the metadata snapshot. No transaction spans
// execution-lock acquisition or release.
func (s *Store) CaptureXRemovalRecordContext(ctx context.Context) (record XRemovalRecord, head string, retErr error) {
	if s.syncGeneration != nil {
		return record, "", errors.New("X removal authority requires the owner Store")
	}
	sources, err := s.ListSourcesContext(ctx, "x")
	if err != nil {
		return record, "", err
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	ids := make([]int64, len(sources))
	for i, source := range sources {
		ids[i] = source.ID
	}
	if len(ids) > 0 {
		lock, err := s.acquireSyncExecutionLocks(ctx, ids)
		if err != nil {
			return record, "", err
		}
		defer func() { retErr = errors.Join(retErr, s.abandonSyncExecutionLocks(ids, lock)) }()
	}
	record, head, err = s.ExportXRemovalRecordContext(ctx)
	if err != nil {
		return XRemovalRecord{}, "", err
	}
	if len(sources) != len(record.Sources) {
		return XRemovalRecord{}, "", errors.New("X source inventory changed during removal capture; retry while the archive is quiescent")
	}
	for i, source := range sources {
		if source.ID != record.Sources[i].SourceID || source.Identifier != record.Sources[i].OwnerID {
			return XRemovalRecord{}, "", errors.New("X source inventory changed during removal capture; retry while the archive is quiescent")
		}
	}
	return record, head, nil
}

// ValidateXRemovalRecord requires a separately trusted current head and the
// archive identity being restored. It accepts no record-supplied hash claim.
func ValidateXRemovalRecord(record XRemovalRecord, expectedHead, archiveUID string) error {
	if !validXRemovalDigest(expectedHead) || record.ArchiveUID != archiveUID {
		return errors.New("X removal record has no matching current archive authority")
	}
	_, data, err := normalizeXRemovalRecord(record)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expectedHead {
		return errors.New("X removal record does not match the independently trusted current head")
	}
	return nil
}

// ValidateXRemovalRecordContext additionally binds every restored X source to
// the same connection incarnation and financial authority, and preserves all
// suppressions known to the snapshot. Pending cleanup may then replay locally.
func (s *Store) ValidateXRemovalRecordContext(ctx context.Context, record XRemovalRecord, expectedHead string) error {
	if s.syncGeneration != nil {
		return errors.New("X removal authority requires the owner Store")
	}
	return s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		current, err := s.xRemovalRecordTx(ctx, tx, false)
		if err != nil {
			return err
		}
		if err := ValidateXRemovalRecord(record, expectedHead, current.ArchiveUID); err != nil {
			return err
		}
		normalized, _, err := normalizeXRemovalRecord(record)
		if err != nil {
			return err
		}
		bySource := make(map[int64]XRemovalRecordSource, len(normalized.Sources))
		known := make(map[string]bool)
		for _, source := range normalized.Sources {
			bySource[source.SourceID] = source
			for _, id := range source.RemovedIDs {
				known[id] = true
			}
		}
		for _, source := range current.Sources {
			authority, ok := bySource[source.SourceID]
			if !ok || authority.OwnerID != source.OwnerID || authority.InstanceID != source.InstanceID {
				return errors.New("X removal record does not cover the restored source incarnation")
			}
			if authority.FinancialHead != source.FinancialHead {
				return errors.New("X snapshot predates current spending or send authority; create a current snapshot")
			}
			targetHead := source.TargetHead
			if len(known) > 0 {
				targetHead, err = s.xRestoreTargetHeadTx(ctx, tx, source.SourceID, known)
				if err != nil {
					return err
				}
			}
			if authority.TargetHead != targetHead {
				return errors.New("X snapshot does not match current reply-target verification; create a current snapshot")
			}
			delete(bySource, source.SourceID)
			// An alias may be retained by a different source after an isolated
			// restore. Require its global removal authority without weakening
			// the restored account's source/owner/incarnation binding above.
			for _, id := range source.RemovedIDs {
				if !known[id] {
					return errors.New("X removal record omits a suppression known to the snapshot")
				}
			}
		}
		for _, source := range bySource {
			if source.FinancialHead != emptyXRestoreFinancialHead() {
				return errors.New("X snapshot omits a source with spending or send authority; create a current snapshot")
			}
		}
		return nil
	})
}

func (s *Store) xRemovalRecordTx(ctx context.Context, tx *loggedTx, requireComplete bool) (XRemovalRecord, error) {
	record := XRemovalRecord{Version: 3, Sources: []XRemovalRecordSource{}}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, archiveUIDKey).Scan(&record.ArchiveUID); err != nil {
		return record, err
	}
	if requireComplete {
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM x_post_versions WHERE cleanup_pending<>0)`).Scan(&pending); err != nil {
			return record, err
		}
		if pending {
			return record, errors.New("X removal record is unavailable while content cleanup is pending")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,identifier FROM sources WHERE source_type='x' ORDER BY id`)
	if err != nil {
		return record, err
	}
	for rows.Next() {
		var source XRemovalRecordSource
		if err := rows.Scan(&source.SourceID, &source.OwnerID); err != nil {
			_ = rows.Close()
			return record, err
		}
		record.Sources = append(record.Sources, source)
		if len(record.Sources) > XMaxRemovalRecordBytes/128 {
			_ = rows.Close()
			return record, errors.New("X removal record exceeds capacity")
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return record, err
	}
	contentBytes := 0
	for i := range record.Sources {
		source := &record.Sources[i]
		state, err := s.loadXControlTx(ctx, tx, source.SourceID)
		if err != nil {
			return record, err
		}
		if !validXRemovalDigest(state.InstanceID) {
			return record, errors.New("X removal record requires verified source identity; reconnect the account")
		}
		source.InstanceID = state.InstanceID
		source.FinancialHead, err = s.xRestoreFinancialHeadTx(ctx, tx, source.SourceID, state)
		if err != nil {
			return record, err
		}
		source.TargetHead, err = s.xRestoreTargetHeadTx(ctx, tx, source.SourceID, nil)
		if err != nil {
			return record, err
		}
		source.RemovedIDs = []string{}
		aliases, err := tx.QueryContext(ctx, `SELECT post_id FROM x_post_versions WHERE source_id=? AND suppressed_at IS NOT NULL ORDER BY LENGTH(post_id),post_id`, source.SourceID)
		if err != nil {
			return record, err
		}
		for aliases.Next() {
			var id string
			if err := aliases.Scan(&id); err != nil {
				_ = aliases.Close()
				return record, err
			}
			contentBytes += len(id) + 3
			if contentBytes > XMaxRemovalRecordBytes {
				_ = aliases.Close()
				return record, errors.New("X removal record exceeds capacity")
			}
			source.RemovedIDs = append(source.RemovedIDs, id)
		}
		if err := errors.Join(aliases.Err(), aliases.Close()); err != nil {
			return record, err
		}
	}
	return record, nil
}

func normalizeXRemovalRecord(record XRemovalRecord) (XRemovalRecord, []byte, error) {
	invalid := errors.New("invalid or incomplete X removal record")
	if record.Version != 3 || !validXRemovalDigest(record.ArchiveUID) || len(record.Sources) > XMaxRemovalRecordBytes/128 {
		return XRemovalRecord{}, nil, invalid
	}
	result := record
	result.Sources = make([]XRemovalRecordSource, len(record.Sources))
	owners := make(map[string]bool, len(record.Sources))
	ids := make(map[int64]bool, len(record.Sources))
	contentBytes := 0
	for i, source := range record.Sources {
		if source.SourceID <= 0 || !validXControlID(source.OwnerID) || !validXRemovalDigest(source.InstanceID) || !validXRemovalDigest(source.FinancialHead) || !validXRemovalDigest(source.TargetHead) || owners[source.OwnerID] || ids[source.SourceID] {
			return XRemovalRecord{}, nil, invalid
		}
		owners[source.OwnerID], ids[source.SourceID] = true, true
		if len(source.RemovedIDs) > XMaxRemovalRecordBytes/4 {
			return XRemovalRecord{}, nil, invalid
		}
		source.RemovedIDs = append([]string{}, source.RemovedIDs...)
		slices.SortFunc(source.RemovedIDs, compareXPostIDs)
		for j, id := range source.RemovedIDs {
			if !validXControlID(id) || (j > 0 && source.RemovedIDs[j-1] == id) {
				return XRemovalRecord{}, nil, invalid
			}
			contentBytes += len(id) + 3
			if contentBytes > XMaxRemovalRecordBytes {
				return XRemovalRecord{}, nil, invalid
			}
		}
		result.Sources[i] = source
	}
	slices.SortFunc(result.Sources, func(a, b XRemovalRecordSource) int {
		if a.SourceID < b.SourceID {
			return -1
		}
		if a.SourceID > b.SourceID {
			return 1
		}
		return 0
	})
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > XMaxRemovalRecordBytes {
		return XRemovalRecord{}, nil, invalid
	}
	return result, encoded, nil
}

func validXRemovalDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// HasXSourcesContext checks whether an isolated restored archive needs current
// X removal authority before it can be published. It does not change schema.
func (s *Store) HasXSourcesContext(ctx context.Context) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sources WHERE source_type='x')`).Scan(&found)
	return found, err
}
