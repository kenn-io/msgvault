package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/contactcadence"
)

// DirectoryCadence derives the current list without persisting another schedule.
type DirectoryCadence struct {
	FrequencyDays *int64     `json:"frequency_days,omitempty"`
	List          string     `json:"list,omitempty"`
	DueAt         *time.Time `json:"due_at,omitempty"`
	Status        string     `json:"status"`
	DaysOverdue   *int       `json:"days_overdue,omitempty"`
	Stale         bool       `json:"stale"`
}

// DirectoryCadenceCoverage describes the matching roster before cadence filtering.
type DirectoryCadenceCoverage struct {
	Total     int `json:"total"`
	NoList    int `json:"no_list"`
	Unknown   int `json:"unknown"`
	Scheduled int `json:"scheduled"`
}

// DirectoryReachOutContext is optional, independent context for integrations.
// Counts in either direction retain their own source's freshness signal.
type DirectoryReachOutContext struct {
	AwaitingReplyCount *int   `json:"awaiting_reply_count,omitempty"`
	NeedsReplyCount    *int   `json:"needs_reply_count,omitempty"`
	OpenLoopsStale     *bool  `json:"open_loops_stale,omitempty"`
	SuggestedList      string `json:"suggested_list,omitempty"`
	SignalsStale       *bool  `json:"signals_stale,omitempty"`
}

func directoryCadenceSort(order string) bool {
	return order == DirectoryPeopleSortMostOverdue || order == DirectoryPeopleSortNextDue
}

func directoryCadenceDueWithinDays(dueAt *time.Time, now time.Time, days int) bool {
	if dueAt == nil {
		return false
	}
	horizonDate := now.UTC().AddDate(0, 0, days)
	exclusiveCutoff := time.Date(horizonDate.Year(), horizonDate.Month(), horizonDate.Day()+1, 0, 0, 0, 0, time.UTC)
	return dueAt.Before(exclusiveCutoff)
}

func directoryCadence(state ContactState, values []contactFrequencyValue, now time.Time) DirectoryCadence {
	result := DirectoryCadence{Status: CadenceUnknown, Stale: state.Stale}
	if len(values) != 1 || !values[0].Valid || values[0].Days <= 0 {
		return result
	}
	days := values[0].Days
	result.FrequencyDays = &days
	result.List = contactcadence.ListForDays(days)
	if state.Stale {
		return result
	}
	deriveContactCadence(&state, values, now)
	result.DueAt, result.Status = state.CadenceDueAt, state.CadenceStatus
	if state.CadenceStatus == CadenceOverdue {
		elapsed := now.Unix() - state.CadenceDueAt.Unix()
		if now.Nanosecond() < state.CadenceDueAt.Nanosecond() {
			elapsed--
		}
		overdue := int(elapsed / 86400)
		result.DaysOverdue = &overdue
	}
	return result
}

// hydrateDirectoryCadenceTx reads bounded metadata batches, never message bodies
// or per-person contact-state queries. The global projection barrier is read once.
func (s *Store) hydrateDirectoryCadenceTx(ctx context.Context, tx *loggedTx, candidates []directoryPersonCandidate, now time.Time) error {
	if len(candidates) == 0 {
		return nil
	}
	revisions, globalStale, err := s.contactStateFreshnessTxContext(ctx, tx)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(candidates, 100) {
		ids := make([]any, len(chunk))
		byID := make(map[int64]int, len(chunk))
		for i, c := range chunk {
			ids[i] = c.summary.ID
			byID[c.summary.ID] = i
		}
		frequencies := make(map[int64][]contactFrequencyValue, len(chunk))
		rows, err := tx.QueryContext(ctx, `SELECT v.person_id, d.value_type,
   v.value_text, CAST(v.value_integer AS TEXT), CAST(v.value_real AS TEXT),
   CAST(v.value_boolean AS TEXT), v.value_date, CAST(v.value_timestamp AS TEXT),
   CAST(v.value_json AS TEXT), v.value_record_type, CAST(v.value_record_id AS TEXT)
   FROM person_attribute_values v JOIN attribute_definitions d ON d.id=v.definition_id
   WHERE v.person_id IN (`+directoryPlaceholders(len(ids))+`) AND d.universal_id=?
   AND d.object_type='person' AND v.active_until IS NULL AND v.superseded_at IS NULL`, append(append([]any{}, ids...), AttributeUniversalIDContactFrequency)...)
		if err != nil {
			return fmt.Errorf("read directory frequencies: %w", err)
		}
		for rows.Next() {
			var id int64
			var kind string
			var fields [9]sql.NullString
			if err := rows.Scan(&id, &kind, &fields[0], &fields[1], &fields[2], &fields[3], &fields[4], &fields[5], &fields[6], &fields[7], &fields[8]); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan directory frequency: %w", err)
			}
			frequencies[id] = append(frequencies[id], decodeContactFrequency(kind, fields))
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("iterate directory frequencies: %w", err)
		}
		rows, err = tx.QueryContext(ctx, `SELECT p.id, cs.person_id, cs.last_contact_at, cs.last_contact_channel,
   cs.identity_revision, cs.account_identity_revision, cs.dirty_at,
   CASE WHEN EXISTS (SELECT 1 FROM activity_event_persons aep WHERE aep.person_id=p.id AND aep.evidence='direct') THEN 1 ELSE 0 END
   FROM persons p LEFT JOIN person_contact_state cs ON cs.person_id=p.id
   WHERE p.id IN (`+directoryPlaceholders(len(ids))+`)`, ids...)
		if err != nil {
			return fmt.Errorf("read directory contact metadata: %w", err)
		}
		for rows.Next() {
			var id int64
			var stored, identity, account sql.NullInt64
			var last, dirty sql.NullTime
			var channel sql.NullString
			var direct int
			if err := rows.Scan(&id, &stored, &last, &channel, &identity, &account, &dirty, &direct); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan directory contact metadata: %w", err)
			}
			stale := globalStale
			if stored.Valid {
				stale = stale || dirty.Valid || identity.Int64 != revisions.IdentityRevision || account.Int64 != revisions.AccountIdentityRevision
			} else {
				stale = stale || direct != 0
			}
			person := &chunk[byID[id]].summary
			person.Cadence = directoryCadence(ContactState{LastContactAt: utcNullTimePointer(last), Stale: stale}, frequencies[id], now)
			person.LastContactChannel = channel.String
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("iterate directory contact metadata: %w", err)
		}
	}
	return nil
}

func (s *Store) directoryCadencePageTx(ctx context.Context, tx *loggedTx, query normalizedDirectoryPeopleQuery, after *directoryPeopleCursor) (*DirectoryPeoplePage, error) {
	now := time.Now().UTC()
	if after != nil {
		var err error
		now, err = time.Parse(time.RFC3339Nano, after.EvaluatedAt)
		if err != nil || now.Year() < 0 || now.Year() > 9999 {
			return nil, ErrInvalidDirectoryCursor
		}
	}
	scan := query
	scan.all = true
	if directoryCadenceSort(scan.sort) {
		scan.sort = DirectoryPeopleSortName
	}
	candidates, err := s.selectDirectoryPeopleTx(ctx, tx, scan, nil)
	if err != nil {
		return nil, err
	}
	if err = s.hydrateDirectoryCadenceTx(ctx, tx, candidates, now); err != nil {
		return nil, err
	}
	coverage := &DirectoryCadenceCoverage{Total: len(candidates)}
	eligible := make([]directoryPersonCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		cadence := candidate.summary.Cadence
		switch {
		case cadence.FrequencyDays == nil:
			coverage.NoList++
		case cadence.DueAt == nil:
			coverage.Unknown++
		default:
			coverage.Scheduled++
		}
		matches := query.cadence == ""
		switch query.cadence {
		case "overdue":
			matches = cadence.Status == CadenceOverdue
		case "due":
			matches = directoryCadenceDueWithinDays(cadence.DueAt, now, query.dueWithinDays)
		case "no_list":
			matches = cadence.FrequencyDays == nil
		case "unknown":
			matches = cadence.FrequencyDays != nil && cadence.DueAt == nil
		}
		if matches {
			if directoryCadenceSort(query.sort) {
				candidate.sortKey = "~" // Known dates precede unknown dates.
				if cadence.DueAt != nil {
					candidate.sortKey = directoryLastContactKey(*cadence.DueAt)
				}
			}
			eligible = append(eligible, candidate)
		}
	}
	if directoryCadenceSort(query.sort) {
		slices.SortFunc(eligible, func(a, b directoryPersonCandidate) int {
			if c := strings.Compare(a.sortKey, b.sortKey); c != 0 {
				return c
			}
			if c := strings.Compare(a.orderName, b.orderName); c != 0 {
				return c
			}
			if a.summary.ID < b.summary.ID {
				return -1
			}
			if a.summary.ID > b.summary.ID {
				return 1
			}
			return 0
		})
	}
	if after != nil {
		index := slices.IndexFunc(eligible, func(c directoryPersonCandidate) bool { return c.summary.ID == after.PersonID })
		if index < 0 || after.Quality != eligible[index].quality || after.AnchorHash != directoryCadenceAnchorHash(eligible[index]) {
			return nil, ErrInvalidDirectoryCursor
		}
		eligible = eligible[index+1:]
	}
	page := &DirectoryPeoplePage{People: []DirectoryPersonSummary{}, CadencePresets: contactcadence.Presets(), CadenceCoverage: coverage}
	if len(eligible) > query.limit {
		last := eligible[query.limit-1]
		page.NextCursor, err = encodeDirectoryPeopleCursor(directoryPeopleCursor{Version: directoryCursorVersion, Fingerprint: query.fingerprint, Quality: last.quality, PersonID: last.summary.ID, AnchorHash: directoryCadenceAnchorHash(last), EvaluatedAt: now.Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		eligible = eligible[:query.limit]
	}
	if err = s.hydrateDirectoryPeopleTx(ctx, tx, eligible); err != nil {
		return nil, err
	}
	for _, c := range eligible {
		page.People = append(page.People, c.summary)
	}
	return page, nil
}

func directoryCadenceAnchorHash(c directoryPersonCandidate) string {
	cadence := c.summary.Cadence
	key := fmt.Sprintf("%s/%v/%t/", c.sortKey, cadence.FrequencyDays != nil, cadence.Stale)
	if cadence.FrequencyDays != nil {
		key += strconv.FormatInt(*cadence.FrequencyDays, 10)
	}
	if cadence.DueAt != nil {
		key += "/" + directoryLastContactKey(*cadence.DueAt)
	}
	return directoryAnchorHash(key, c.orderName)
}
