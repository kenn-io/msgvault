package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// ImportantDateLineage is an owner-only inspection of connected identity
// history. It does not approve any mapping or release a quarantined identity.
type ImportantDateLineage struct {
	ActiveReconciliations []string                       `json:"active_reconciliations"`
	TopologyDigest        string                         `json:"topology_digest"`
	Digest                string                         `json:"digest,omitempty"`
	IdentityRevision      int64                          `json:"identity_revision"`
	PersonIDs             []int64                        `json:"person_ids"`
	MissingPersonIDs      []int64                        `json:"missing_person_ids"`
	PersonUIDs            []string                       `json:"person_uids"`
	People                []ImportantDateLineagePerson   `json:"people"`
	Aliases               []ImportantDateLineageAlias    `json:"aliases"`
	MergeSnapshots        []ImportantDateLineageSnapshot `json:"merge_snapshots"`
	MergeIDs              []int64                        `json:"merge_ids"`
	SplitResults          []PersonSplitResult            `json:"split_results"`
	MissingSplitIDs       []int64                        `json:"missing_split_result_ids"`
	SplitIDs              []int64                        `json:"split_ids"`
	OwnedSubjects         []ImportantDateLineageSubject  `json:"owned_subjects"`
	Deliveries            []ImportantDateLineageDelivery `json:"deliveries"`
	Guards                []ImportantDateLineageGuard    `json:"guards"`
}

type ImportantDateLineageGuard struct {
	ReconciliationUID   string `json:"reconciliation_uid"`
	ResolvedSubject     string `json:"resolved_subject"`
	Meaning             string `json:"meaning"`
	OccurrenceYear      int    `json:"occurrence_year"`
	OriginalKey         string `json:"original_occurrence_key"`
	AcceptedFingerprint string `json:"accepted_fingerprint,omitempty"`
}

type ImportantDateLineageSnapshot struct {
	MergeID int64  `json:"merge_id"`
	SHA256  string `json:"sha256"`
}

type ImportantDateLineageDelivery struct {
	OccurrenceKey       string                     `json:"occurrence_key"`
	State               ImportantDateDeliveryState `json:"state"`
	IssueUID            string                     `json:"issue_uid,omitempty"`
	Fingerprint         string                     `json:"fingerprint"`
	AcceptedFingerprint string                     `json:"accepted_fingerprint,omitempty"`
}

type ImportantDateLineageAlias struct {
	UID           string `json:"uid"`
	OwnerPersonID int64  `json:"owner_person_id"`
}

type ImportantDateLineagePerson struct {
	ID                 int64                    `json:"id"`
	UID                string                   `json:"uid"`
	Revision           int64                    `json:"revision"`
	ProjectionRevision int64                    `json:"projection_revision"`
	Dates              []PersonDate             `json:"dates"`
	ContactFrequency   []PersonAttributeValue   `json:"contact_frequency"`
	Relationships      []PersonRelationshipView `json:"relationships"`
}

type ImportantDateLineageSubject struct {
	UID         string                                `json:"uid"`
	OwnerUID    string                                `json:"owner_uid"`
	Kind        string                                `json:"kind"`
	DisplayName string                                `json:"display_name"`
	Date        PartialDate                           `json:"date"`
	Revision    int64                                 `json:"revision"`
	Withdrawn   bool                                  `json:"withdrawn"`
	History     []ImportantDateLineageSubjectRevision `json:"history"`
}

type ImportantDateLineageSubjectRevision struct {
	Revision    int64       `json:"revision"`
	DisplayName string      `json:"display_name"`
	Date        PartialDate `json:"date"`
	Withdrawn   bool        `json:"withdrawn"`
}

// ImportantDateLineageContext follows both sides of every connected merge and
// split, including retired identities. All rows come from one read snapshot.
// A later identity revision invalidates any decisions made from this view.
func (s *Store) ImportantDateLineageContext(ctx context.Context, personID int64) (ImportantDateLineage, error) {
	var result ImportantDateLineage
	if personID <= 0 {
		return result, ErrPersonNotFound
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	snapshot, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	if !ok || snapshot.db != s.db.DB {
		return result, errors.New("important-date lineage snapshot unavailable")
	}
	if _, err := s.GetPersonContext(ctx, personID); err != nil {
		return result, err
	}
	result.IdentityRevision, err = s.IdentityRevisionContext(ctx)
	if err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE edges(a, b) AS (
		SELECT survivor_person_id_at_merge, absorbed_person_id FROM person_merges
		UNION SELECT survivor_person_id_at_merge, current_person_id FROM person_merges WHERE current_person_id IS NOT NULL
		UNION SELECT source_person_id, new_person_id FROM person_splits
	), lineage(id) AS (
		SELECT CAST(? AS BIGINT) UNION SELECT CASE WHEN edges.a = lineage.id THEN edges.b ELSE edges.a END
		FROM lineage JOIN edges ON edges.a = lineage.id OR edges.b = lineage.id
	) SELECT id FROM lineage ORDER BY id`, personID)
	if err != nil {
		return result, fmt.Errorf("read important-date identity lineage: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("scan important-date lineage: %w", err)
		}
		result.PersonIDs = append(result.PersonIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, fmt.Errorf("iterate important-date lineage: %w", err)
	}
	if err := rows.Close(); err != nil {
		return result, fmt.Errorf("close important-date lineage: %w", err)
	}
	uids := map[string]struct{}{}
	merges := map[int64]string{}
	splits := map[int64]*PersonSplitResult{}
	for _, id := range result.PersonIDs {
		var uid string
		var revision, projectionRevision int64
		err := s.db.QueryRowContext(ctx, `SELECT vcard_uid, revision, vcard_projection_revision FROM persons WHERE id = ?`, id).Scan(&uid, &revision, &projectionRevision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("read lineage person: %w", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			result.MissingPersonIDs = append(result.MissingPersonIDs, id)
		}
		if err == nil {
			uids[uid] = struct{}{}
			dates, err := s.listPersonDatesTx(ctx, snapshot.tx, id, false)
			if err != nil {
				return result, fmt.Errorf("read lineage dates: %w", err)
			}
			frequency, err := s.ListPersonAttributeValuesContext(ctx, id, PersonAttributeQuery{DefinitionSlug: AttributeSlugContactFrequency})
			if err != nil {
				return result, fmt.Errorf("read lineage contact frequency: %w", err)
			}
			relationships, err := s.ListPersonRelationshipsContext(ctx, id, PersonRelationshipListOptions{})
			if err != nil {
				return result, fmt.Errorf("read lineage relationships: %w", err)
			}
			result.People = append(result.People, ImportantDateLineagePerson{ID: id, UID: uid, Revision: revision, ProjectionRevision: projectionRevision, Dates: dates, ContactFrequency: frequency, Relationships: relationships})
		}
		rows, err := s.db.QueryContext(ctx, `SELECT retired_uid FROM person_uid_aliases WHERE surviving_person_id = ?`, id)
		if err != nil {
			return result, fmt.Errorf("read lineage aliases: %w", err)
		}
		for rows.Next() {
			var alias string
			if err := rows.Scan(&alias); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage alias: %w", err)
			}
			uids[alias] = struct{}{}
			result.Aliases = append(result.Aliases, ImportantDateLineageAlias{UID: alias, OwnerPersonID: id})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate lineage aliases: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close lineage aliases: %w", err)
		}
		rows, err = s.db.QueryContext(ctx, `SELECT id, survivor_uid, absorbed_uid, snapshot_sha256 FROM person_merges WHERE survivor_person_id_at_merge = ? OR absorbed_person_id = ? OR current_person_id = ?`, id, id, id)
		if err != nil {
			return result, fmt.Errorf("read lineage merges: %w", err)
		}
		for rows.Next() {
			var mergeID int64
			var survivor, absorbed, snapshotDigest string
			if err := rows.Scan(&mergeID, &survivor, &absorbed, &snapshotDigest); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage merge: %w", err)
			}
			merges[mergeID] = snapshotDigest
			uids[survivor] = struct{}{}
			uids[absorbed] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate lineage merges: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close lineage merges: %w", err)
		}
		rows, err = s.db.QueryContext(ctx, `SELECT id, new_person_uid, result_json FROM person_splits WHERE source_person_id = ? OR new_person_id = ?`, id, id)
		if err != nil {
			return result, fmt.Errorf("read lineage splits: %w", err)
		}
		for rows.Next() {
			var splitID int64
			var newUID string
			var storedResult sql.NullString
			if err := rows.Scan(&splitID, &newUID, &storedResult); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage split: %w", err)
			}
			var splitResult *PersonSplitResult
			if storedResult.Valid && storedResult.String != "" {
				var value PersonSplitResult
				if err := json.Unmarshal([]byte(storedResult.String), &value); err != nil {
					_ = rows.Close()
					return result, fmt.Errorf("decode lineage split outcome: %w", err)
				}
				if value.Split.ID != splitID {
					_ = rows.Close()
					return result, errors.New("lineage split outcome identity mismatch")
				}
				splitResult = &value
			}
			splits[splitID] = splitResult
			uids[newUID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate lineage splits: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close lineage splits: %w", err)
		}
	}
	for uid := range uids {
		if uid != "" {
			result.PersonUIDs = append(result.PersonUIDs, uid)
		}
	}
	for id, digest := range merges {
		result.MergeIDs = append(result.MergeIDs, id)
		result.MergeSnapshots = append(result.MergeSnapshots, ImportantDateLineageSnapshot{MergeID: id, SHA256: digest})
	}
	for id, outcome := range splits {
		result.SplitIDs = append(result.SplitIDs, id)
		if outcome == nil {
			result.MissingSplitIDs = append(result.MissingSplitIDs, id)
		} else {
			result.SplitResults = append(result.SplitResults, *outcome)
		}
	}
	sort.Strings(result.PersonUIDs)
	sort.Slice(result.Aliases, func(i, j int) bool { return result.Aliases[i].UID < result.Aliases[j].UID })
	for _, uid := range result.PersonUIDs {
		rows, err := s.db.QueryContext(ctx, `SELECT uid, owner_uid, kind, display_name, birth_year, birth_month, birth_day, revision, withdrawn FROM important_date_subjects WHERE owner_uid = ? ORDER BY uid`, uid)
		if err != nil {
			return result, fmt.Errorf("read lineage owned subjects: %w", err)
		}
		for rows.Next() {
			var subject ImportantDateLineageSubject
			var year sql.NullInt64
			var month, day, withdrawn int
			if err := rows.Scan(&subject.UID, &subject.OwnerUID, &subject.Kind, &subject.DisplayName, &year, &month, &day, &subject.Revision, &withdrawn); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage owned subject: %w", err)
			}
			subject.Date = PartialDate{Month: &month, Day: &day}
			if year.Valid {
				value := int(year.Int64)
				subject.Date.Year = &value
			}
			subject.Withdrawn = withdrawn != 0
			result.OwnedSubjects = append(result.OwnedSubjects, subject)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate lineage owned subjects: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close lineage owned subjects: %w", err)
		}
	}
	for i := range result.OwnedSubjects {
		subject := &result.OwnedSubjects[i]
		rows, err := s.db.QueryContext(ctx, `SELECT revision, display_name, birth_year, birth_month, birth_day, withdrawn FROM important_date_subject_history WHERE subject_uid = ? ORDER BY revision`, subject.UID)
		if err != nil {
			return result, fmt.Errorf("read owned-subject history: %w", err)
		}
		for rows.Next() {
			var previous ImportantDateLineageSubjectRevision
			var year sql.NullInt64
			var month, day, withdrawn int
			if err := rows.Scan(&previous.Revision, &previous.DisplayName, &year, &month, &day, &withdrawn); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan owned-subject history: %w", err)
			}
			previous.Date = PartialDate{Month: &month, Day: &day}
			if year.Valid {
				value := int(year.Int64)
				previous.Date.Year = &value
			}
			previous.Withdrawn = withdrawn != 0
			subject.History = append(subject.History, previous)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate owned-subject history: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close owned-subject history: %w", err)
		}
	}
	var prefixes []string
	for _, uid := range result.PersonUIDs {
		prefixes = append(prefixes, "person/"+uid+":")
	}
	for _, subject := range result.OwnedSubjects {
		prefixes = append(prefixes, "owned/"+subject.UID+":")
	}
	active := map[string]struct{}{}
	for _, prefix := range prefixes {
		subject := strings.TrimSuffix(prefix, ":")
		rows, err := s.db.QueryContext(ctx, `SELECT r.uid FROM important_date_reconciliations r WHERE r.active = 1
			AND EXISTS (SELECT 1 FROM important_date_identity_bindings b WHERE b.reconciliation_uid = r.uid AND b.original_subject = ?)`, subject)
		if err != nil {
			return result, fmt.Errorf("read active lineage decisions: %w", err)
		}
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan active lineage decision: %w", err)
			}
			active[uid] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate active lineage decisions: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close active lineage decisions: %w", err)
		}
	}
	for uid := range active {
		result.ActiveReconciliations = append(result.ActiveReconciliations, uid)
	}
	sort.Strings(result.ActiveReconciliations)
	for _, prefix := range prefixes {
		rows, err := s.db.QueryContext(ctx, `SELECT occurrence_key, state, issue_uid, fingerprint, accepted_fingerprint FROM important_date_deliveries WHERE substr(occurrence_key, 1, ?) = ? ORDER BY occurrence_key`, utf8.RuneCountInString(prefix), prefix)
		if err != nil {
			return result, fmt.Errorf("read lineage deliveries: %w", err)
		}
		for rows.Next() {
			var item ImportantDateLineageDelivery
			if err := rows.Scan(&item.OccurrenceKey, &item.State, &item.IssueUID, &item.Fingerprint, &item.AcceptedFingerprint); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage delivery: %w", err)
			}
			// Person UIDs may contain colons. Match the full subject after
			// removing the fixed kind and year suffixes, not just a prefix.
			yearAt := strings.LastIndexByte(item.OccurrenceKey, ':')
			kindAt := -1
			if yearAt > 0 {
				kindAt = strings.LastIndexByte(item.OccurrenceKey[:yearAt], ':')
			}
			if kindAt >= 0 && item.OccurrenceKey[:kindAt]+":" == prefix {
				result.Deliveries = append(result.Deliveries, item)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate lineage deliveries: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close lineage deliveries: %w", err)
		}
	}
	sort.Slice(result.Deliveries, func(i, j int) bool { return result.Deliveries[i].OccurrenceKey < result.Deliveries[j].OccurrenceKey })
	for _, delivery := range result.Deliveries {
		rows, err := s.db.QueryContext(ctx, `SELECT reconciliation_uid, resolved_subject, meaning, occurrence_year, accepted_fingerprint
			FROM important_date_occurrence_guards WHERE original_occurrence_key = ? ORDER BY reconciliation_uid, resolved_subject, meaning, occurrence_year`, delivery.OccurrenceKey)
		if err != nil {
			return result, fmt.Errorf("read lineage occurrence guards: %w", err)
		}
		for rows.Next() {
			guard := ImportantDateLineageGuard{OriginalKey: delivery.OccurrenceKey}
			if err := rows.Scan(&guard.ReconciliationUID, &guard.ResolvedSubject, &guard.Meaning, &guard.OccurrenceYear, &guard.AcceptedFingerprint); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage occurrence guard: %w", err)
			}
			result.Guards = append(result.Guards, guard)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("iterate lineage occurrence guards: %w", err)
		}
		if err := rows.Close(); err != nil {
			return result, fmt.Errorf("close lineage occurrence guards: %w", err)
		}
	}
	slices.Sort(result.MergeIDs)
	sort.Slice(result.MergeSnapshots, func(i, j int) bool { return result.MergeSnapshots[i].MergeID < result.MergeSnapshots[j].MergeID })
	slices.Sort(result.SplitIDs)
	slices.Sort(result.MissingSplitIDs)
	sort.Slice(result.SplitResults, func(i, j int) bool { return result.SplitResults[i].Split.ID < result.SplitResults[j].Split.ID })
	topology := struct {
		PersonIDs        []int64                        `json:"person_ids"`
		PersonUIDs       []string                       `json:"person_uids"`
		MissingPersonIDs []int64                        `json:"missing_person_ids"`
		Aliases          []ImportantDateLineageAlias    `json:"aliases"`
		MergeSnapshots   []ImportantDateLineageSnapshot `json:"merge_snapshots"`
		SplitResults     []PersonSplitResult            `json:"split_results"`
		MissingSplitIDs  []int64                        `json:"missing_split_result_ids"`
	}{PersonIDs: result.PersonIDs, PersonUIDs: result.PersonUIDs, MissingPersonIDs: result.MissingPersonIDs,
		Aliases: result.Aliases, MergeSnapshots: result.MergeSnapshots, SplitResults: result.SplitResults, MissingSplitIDs: result.MissingSplitIDs}
	topologyBytes, err := json.Marshal(topology, json.Deterministic(true))
	if err != nil {
		return ImportantDateLineage{}, fmt.Errorf("encode important-date topology: %w", err)
	}
	topologyHash := sha256.Sum256(topologyBytes)
	result.TopologyDigest = hex.EncodeToString(topologyHash[:])
	encoded, err := json.Marshal(result, json.Deterministic(true))
	if err != nil {
		return ImportantDateLineage{}, fmt.Errorf("encode important-date lineage: %w", err)
	}
	digest := sha256.Sum256(encoded)
	result.Digest = hex.EncodeToString(digest[:])
	return result, nil
}
