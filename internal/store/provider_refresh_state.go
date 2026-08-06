package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// providerIdentityRefreshKeyPrefix namespaces the per-source provider refresh
// state rows in archive_metadata. Like the identity discovery backlog, the row
// is only ever read and written on its own, so it stays outside the
// identity-revision lock ordering documented in participant_links.go.
const providerIdentityRefreshKeyPrefix = "provider_identity_refresh:"

// ProviderIdentityRefreshState records the outcome of the most recent
// automatic provider identity refresh for a source. Provider inventory can
// change while a mailbox is idle, so no-op syncs use this state to decide
// whether the provider must be consulted again: a missing or failed state is
// always due, and even a successful one goes stale.
type ProviderIdentityRefreshState struct {
	LastSuccessAt time.Time
	LastError     string
	FailedAt      time.Time
}

// Fresh reports whether the last refresh succeeded recently enough that a
// no-op sync may skip the provider round trip.
func (s ProviderIdentityRefreshState) Fresh(now time.Time, maxAge time.Duration) bool {
	if s.LastError != "" || s.LastSuccessAt.IsZero() {
		return false
	}
	return now.Sub(s.LastSuccessAt) < maxAge
}

// providerIdentityRefreshMarker is the stored JSON shape of
// ProviderIdentityRefreshState. Timestamps travel as RFC 3339 strings to match
// the identity discovery backlog marker.
type providerIdentityRefreshMarker struct {
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	FailedAt      string `json:"failed_at,omitempty"`
}

func providerIdentityRefreshKey(sourceID int64) string {
	return providerIdentityRefreshKeyPrefix + strconv.FormatInt(sourceID, 10)
}

var errProviderRefreshSourceID = errors.New("source ID must be positive")

// RecordProviderIdentityRefreshOutcomeContext stores the result of one
// automatic provider identity refresh attempt. A nil refreshErr records a
// success and clears any previous failure; a non-nil refreshErr records the
// failure while preserving the last success time, so staleness keeps being
// measured from the last refresh that actually worked.
func (s *Store) RecordProviderIdentityRefreshOutcomeContext(
	ctx context.Context,
	sourceID int64,
	refreshErr error,
) error {
	if sourceID <= 0 {
		return fmt.Errorf("record provider identity refresh outcome: %w", errProviderRefreshSourceID)
	}
	key := providerIdentityRefreshKey(sourceID)
	now := time.Now().UTC().Format(time.RFC3339)

	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '')`), key,
		); err != nil {
			return fmt.Errorf("seed provider identity refresh state: %w", err)
		}
		// Read the previous value back through an UPDATE so the row is locked
		// before the new state is derived from it: a failure must not clobber
		// the last success time a concurrent writer just recorded.
		var previous string
		if err := tx.QueryRowContext(ctx,
			`UPDATE archive_metadata SET value = value WHERE key = ? RETURNING value`, key,
		).Scan(&previous); err != nil {
			return fmt.Errorf("lock provider identity refresh state: %w", err)
		}

		marker := providerIdentityRefreshMarker{LastSuccessAt: now}
		if refreshErr != nil {
			// An unreadable previous value drops the remembered success time
			// instead of failing the caller: the state is a scheduling hint,
			// not a ledger, and losing it only causes one extra refresh.
			var prior providerIdentityRefreshMarker
			_ = json.Unmarshal([]byte(previous), &prior)
			marker = providerIdentityRefreshMarker{
				LastSuccessAt: prior.LastSuccessAt,
				LastError:     refreshErr.Error(),
				FailedAt:      now,
			}
		}
		encoded, err := json.Marshal(marker)
		if err != nil {
			return fmt.Errorf("encode provider identity refresh state: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE archive_metadata SET value = ? WHERE key = ?`, string(encoded), key,
		); err != nil {
			return fmt.Errorf("update provider identity refresh state: %w", err)
		}
		return nil
	})
}

// ProviderIdentityRefreshStateContext returns the recorded refresh state for
// sourceID. A source with no record reports found=false, which callers treat
// as due. An undecodable payload reports found=true with a zero state — also
// due — so a corrupted row heals itself on the next successful refresh.
func (s *Store) ProviderIdentityRefreshStateContext(
	ctx context.Context,
	sourceID int64,
) (state ProviderIdentityRefreshState, found bool, err error) {
	if sourceID <= 0 {
		return ProviderIdentityRefreshState{}, false,
			fmt.Errorf("read provider identity refresh state: %w", errProviderRefreshSourceID)
	}
	var value string
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, providerIdentityRefreshKey(sourceID),
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderIdentityRefreshState{}, false, nil
	}
	if err != nil {
		return ProviderIdentityRefreshState{}, false,
			fmt.Errorf("read provider identity refresh state: %w", err)
	}

	var marker providerIdentityRefreshMarker
	decodable := json.Unmarshal([]byte(value), &marker) == nil
	if !decodable {
		return ProviderIdentityRefreshState{}, true, nil
	}
	state = ProviderIdentityRefreshState{LastError: marker.LastError}
	if t, parseErr := time.Parse(time.RFC3339, marker.LastSuccessAt); parseErr == nil {
		state.LastSuccessAt = t
	}
	if t, parseErr := time.Parse(time.RFC3339, marker.FailedAt); parseErr == nil {
		state.FailedAt = t
	}
	return state, true, nil
}
