package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

// ListKeepInTouchPeopleContext walks saved people by stable ID without
// depending on the Directory's derived projection. ContactStateContext owns
// the counted-contact and freshness decision for each returned person.
func (s *Store) ListKeepInTouchPeopleContext(ctx context.Context, afterID int64, limit int) ([]Person, error) {
	if afterID < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("keep-in-touch page requires a nonnegative cursor and limit 1..100")
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT id, vcard_uid, display_name FROM persons WHERE id > ? ORDER BY id LIMIT ?`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list keep-in-touch people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	people := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.VCardUID, &p.DisplayName); err != nil {
			return nil, fmt.Errorf("read keep-in-touch person: %w", err)
		}
		people = append(people, p)
	}
	return people, rows.Err()
}

// KeepInTouchContactStateContext derives cadence from the same counted rows
// and exclusions as people tiers, across the complete archive rather than a
// tier's rolling window. The broader activity contact projection supplies the
// freshness barrier; mailing lists, unsent records and incidental activity do
// not get to advance this reader's contact coordinate.
func (s *Store) KeepInTouchContactStateContext(ctx context.Context, personID int64, now time.Time, options PersonTierOptions) (ContactState, error) {
	var state ContactState
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		state, err = s.contactStateTxContext(ctx, tx, personID, now)
		if err != nil {
			return err
		}
		options.asOf = now
		plan, err := s.planPersonTiers(ctx, tx, options)
		if errors.Is(err, ErrPersonTierRebuilding) {
			state.Stale = true
			return nil
		}
		if err != nil {
			return err
		}
		readiness, err := s.personTierReadiness(ctx, tx)
		if err != nil {
			return err
		}
		if state.Stale || !plan.checkSettings(readiness, options).Ready {
			state.Stale = true
			return nil
		}
		values, err := listCurrentContactFrequencyTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		state.LastContactAt = nil
		state.LastContactRef = ""
		state.LastContactChannel = ""
		state.LastContactSourceID = nil
		// People without a saved positive cadence are ineligible regardless of
		// contact history. Keep the freshness barrier, but avoid paging that
		// history on every daily scan. deriveContactCadence owns date bounds.
		if len(values) != 1 || !values[0].Valid || values[0].Days <= 0 {
			deriveContactCadence(&state, values, now)
			return nil
		}
		latest, found, err := s.lastCountedContactTx(ctx, tx, personID, now, plan, options.Config)
		if err != nil {
			return err
		}
		if found {
			at := latest.OccurredAt.UTC()
			state.LastContactAt = &at
			kind := RefKindMessage
			if latest.Meeting {
				kind = RefKindMeeting
			}
			state.LastContactRef = fmt.Sprintf("%s:%d", kind, latest.MessageID)
			state.LastContactChannel = ActivityChannel(latest.Channel)
			state.LastContactSourceID = &latest.SourceID
		}
		deriveContactCadence(&state, values, now)
		return nil
	})
	return state, err
}

func (s *Store) lastCountedContactTx(ctx context.Context, tx *loggedTx, personID int64, now time.Time, plan personTierPlan, cfg persontiers.Config) (tierSignalRow, bool, error) {
	var latest, after tierSignalRow
	found := false
	for {
		rows, err := s.personTierRowsBetween(ctx, tx, []int64{personID}, "0001-01-01", plan.asOf.Format(time.DateOnly), cfg, after)
		if err != nil {
			return latest, false, err
		}
		broadcast, err := s.personTierBroadcast(ctx, tx, rows, cfg, plan.projection.MaxDirect)
		if err != nil {
			return latest, false, err
		}
		for _, r := range rows {
			if personTierExcludedReason(r, cfg, broadcast) != "" || r.OccurredAt.After(now) {
				continue
			}
			if !found || r.OccurredAt.After(latest.OccurredAt) || r.OccurredAt.Equal(latest.OccurredAt) && r.MessageID > latest.MessageID {
				latest = r
				found = true
			}
		}
		if len(rows) < personTierRowPage {
			return latest, found, nil
		}
		after = rows[len(rows)-1]
	}
}

// KeepInTouchCursorContext reads an optional runtime checkpoint. It neither
// changes the archive schema nor initializes metadata during previews.
func (s *Store) KeepInTouchCursorContext(ctx context.Context) (int64, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), "keep_in_touch.cursor.v1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read outreach checkpoint: %w", err)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10) != raw {
		return 0, errors.New("invalid outreach checkpoint")
	}
	return id, nil
}

// AdvanceKeepInTouchCursorContext preserves another concurrent run's progress.
func (s *Store) AdvanceKeepInTouchCursorContext(ctx context.Context, expected, next int64) (bool, error) {
	if expected < 0 || next < 0 {
		return false, errors.New("invalid outreach checkpoint")
	}
	var query string
	var args []any
	if expected == 0 {
		query = `INSERT INTO archive_metadata (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE archive_metadata.value = '0'`
		args = []any{"keep_in_touch.cursor.v1", strconv.FormatInt(next, 10)}
	} else {
		query = `UPDATE archive_metadata SET value = ? WHERE key = ? AND value = ?`
		args = []any{strconv.FormatInt(next, 10), "keep_in_touch.cursor.v1", strconv.FormatInt(expected, 10)}
	}
	result, err := s.db.ExecContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return false, fmt.Errorf("advance outreach checkpoint: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
