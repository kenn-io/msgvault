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

// identityDiscoveryBacklogKeyPrefix namespaces the per-source backlog rows in
// archive_metadata. The row does not participate in the identity-revision lock
// ordering documented in participant_links.go: it is only ever read and written
// on its own, never alongside an account_identities write.
const identityDiscoveryBacklogKeyPrefix = "identity_discovery_backlog:"

// identityDiscoveryBacklogMarker records that a source owes identity discovery
// a refresh. Discovery evidence is recomputable from the archived messages, so
// a failed sync page parks the debt here instead of failing (and thereby
// unwinding) an otherwise-successful sync run.
type identityDiscoveryBacklogMarker struct {
	LastError string `json:"last_error"`
	FailedAt  string `json:"failed_at"`
	Attempts  int    `json:"attempts"`
}

func identityDiscoveryBacklogKey(sourceID int64) string {
	return identityDiscoveryBacklogKeyPrefix + strconv.FormatInt(sourceID, 10)
}

var errIdentityBacklogSourceID = errors.New("source ID must be positive")

// SetIdentityDiscoveryBacklogContext marks sourceID as owing an identity
// discovery refresh, recording cause and bumping the consecutive-failure count.
func (s *Store) SetIdentityDiscoveryBacklogContext(
	ctx context.Context,
	sourceID int64,
	cause error,
) error {
	if sourceID <= 0 {
		return fmt.Errorf("set identity discovery backlog: %w", errIdentityBacklogSourceID)
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	key := identityDiscoveryBacklogKey(sourceID)

	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '')`), key,
		); err != nil {
			return fmt.Errorf("seed identity discovery backlog: %w", err)
		}
		// Read the previous value back through an UPDATE so the row is locked
		// before the attempt count is derived from it: two concurrent syncs of
		// the same source failing discovery at once must not both read the
		// same count and both store count+1.
		var previous string
		if err := tx.QueryRowContext(ctx,
			`UPDATE archive_metadata SET value = value WHERE key = ? RETURNING value`, key,
		).Scan(&previous); err != nil {
			return fmt.Errorf("lock identity discovery backlog: %w", err)
		}

		marker := identityDiscoveryBacklogMarker{
			LastError: message,
			FailedAt:  time.Now().UTC().Format(time.RFC3339),
			Attempts:  1,
		}
		// An unreadable previous value restarts the count instead of failing
		// the caller: the marker is a repair hint, not a ledger. The seeded
		// empty string takes this path on the first failure.
		var prior identityDiscoveryBacklogMarker
		if json.Unmarshal([]byte(previous), &prior) == nil && prior.Attempts > 0 {
			marker.Attempts = prior.Attempts + 1
		}
		encoded, err := json.Marshal(marker)
		if err != nil {
			return fmt.Errorf("encode identity discovery backlog: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE archive_metadata SET value = ? WHERE key = ?`, string(encoded), key,
		); err != nil {
			return fmt.Errorf("update identity discovery backlog: %w", err)
		}
		return nil
	})
}

// IdentityDiscoveryBacklogContext reports whether sourceID owes an identity
// discovery refresh, along with the error text that parked it. A source with no
// marker is not an error condition.
func (s *Store) IdentityDiscoveryBacklogContext(
	ctx context.Context,
	sourceID int64,
) (found bool, lastError string, err error) {
	if sourceID <= 0 {
		return false, "", fmt.Errorf("read identity discovery backlog: %w", errIdentityBacklogSourceID)
	}
	var value string
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, identityDiscoveryBacklogKey(sourceID),
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("read identity discovery backlog: %w", err)
	}
	// The debt is real even if the payload is not decodable, so a decode miss
	// is a formatting question rather than a read failure: surface the raw
	// value so the drain still runs and the operator sees what is stored.
	var marker identityDiscoveryBacklogMarker
	decodable := json.Unmarshal([]byte(value), &marker) == nil
	if !decodable {
		return true, value, nil
	}
	return true, marker.LastError, nil
}

// ClearIdentityDiscoveryBacklogContext drops the marker after a successful
// refresh. Clearing an absent marker is a no-op.
func (s *Store) ClearIdentityDiscoveryBacklogContext(ctx context.Context, sourceID int64) error {
	if sourceID <= 0 {
		return fmt.Errorf("clear identity discovery backlog: %w", errIdentityBacklogSourceID)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM archive_metadata WHERE key = ?`, identityDiscoveryBacklogKey(sourceID),
	); err != nil {
		return fmt.Errorf("clear identity discovery backlog: %w", err)
	}
	return nil
}
