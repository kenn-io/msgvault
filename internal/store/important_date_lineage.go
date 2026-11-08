package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ImportantDateLineage is an owner-only inspection of connected identity
// history. It does not approve any mapping or release a quarantined identity.
type ImportantDateLineage struct {
	IdentityRevision int64                         `json:"identity_revision"`
	PersonIDs        []int64                       `json:"person_ids"`
	PersonUIDs       []string                      `json:"person_uids"`
	People           []ImportantDateLineagePerson  `json:"people"`
	Aliases          []ImportantDateLineageAlias   `json:"aliases"`
	MergeIDs         []int64                       `json:"merge_ids"`
	SplitIDs         []int64                       `json:"split_ids"`
	OwnedSubjects    []ImportantDateLineageSubject `json:"owned_subjects"`
	Deliveries       []ImportantDateDelivery       `json:"deliveries"`
}

type ImportantDateLineageAlias struct {
	UID           string `json:"uid"`
	OwnerPersonID int64  `json:"owner_person_id"`
}

type ImportantDateLineagePerson struct {
	ID       int64        `json:"id"`
	UID      string       `json:"uid"`
	Revision int64        `json:"revision"`
	Dates    []PersonDate `json:"dates"`
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
	merges := map[int64]struct{}{}
	splits := map[int64]struct{}{}
	for _, id := range result.PersonIDs {
		var uid string
		var revision int64
		err := s.db.QueryRowContext(ctx, `SELECT vcard_uid, revision FROM persons WHERE id = ?`, id).Scan(&uid, &revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("read lineage person: %w", err)
		}
		if err == nil {
			uids[uid] = struct{}{}
			dates, err := s.listPersonDatesTx(ctx, snapshot.tx, id, false)
			if err != nil {
				return result, fmt.Errorf("read lineage dates: %w", err)
			}
			result.People = append(result.People, ImportantDateLineagePerson{ID: id, UID: uid, Revision: revision, Dates: dates})
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
		rows, err = s.db.QueryContext(ctx, `SELECT id, survivor_uid, absorbed_uid FROM person_merges WHERE survivor_person_id_at_merge = ? OR absorbed_person_id = ? OR current_person_id = ?`, id, id, id)
		if err != nil {
			return result, fmt.Errorf("read lineage merges: %w", err)
		}
		for rows.Next() {
			var mergeID int64
			var survivor, absorbed string
			if err := rows.Scan(&mergeID, &survivor, &absorbed); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage merge: %w", err)
			}
			merges[mergeID] = struct{}{}
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
		rows, err = s.db.QueryContext(ctx, `SELECT id, new_person_uid FROM person_splits WHERE source_person_id = ? OR new_person_id = ?`, id, id)
		if err != nil {
			return result, fmt.Errorf("read lineage splits: %w", err)
		}
		for rows.Next() {
			var splitID int64
			var newUID string
			if err := rows.Scan(&splitID, &newUID); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("scan lineage split: %w", err)
			}
			splits[splitID] = struct{}{}
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
	for id := range merges {
		result.MergeIDs = append(result.MergeIDs, id)
	}
	for id := range splits {
		result.SplitIDs = append(result.SplitIDs, id)
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
	for _, prefix := range prefixes {
		rows, err := s.db.QueryContext(ctx, `SELECT occurrence_key, state, issue_uid FROM important_date_deliveries WHERE substr(occurrence_key, 1, ?) = ? ORDER BY occurrence_key`, utf8.RuneCountInString(prefix), prefix)
		if err != nil {
			return result, fmt.Errorf("read lineage deliveries: %w", err)
		}
		for rows.Next() {
			var item ImportantDateDelivery
			if err := rows.Scan(&item.OccurrenceKey, &item.State, &item.IssueUID); err != nil {
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
	sort.Slice(result.MergeIDs, func(i, j int) bool { return result.MergeIDs[i] < result.MergeIDs[j] })
	sort.Slice(result.SplitIDs, func(i, j int) bool { return result.SplitIDs[i] < result.SplitIDs[j] })
	return result, nil
}
