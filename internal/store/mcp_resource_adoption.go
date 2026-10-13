package store

import (
	"context"
	"database/sql"
	"errors"
)

const mcpResourceAdoptionKey = "mcp_events_resource_adoption_v1"

// sealUnusedMCPAdoption consumes upgrade exceptions during writable setup for
// an archive that has never captured Events. Default-off runtime startup can
// then avoid a write lock. A legacy fingerprint, clock or authority row keeps
// the first-configuration adoption transaction intact.
func (s *Store) sealUnusedMCPAdoption(ctx context.Context) error {
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, clock MCPEventClock) error {
		if clock.Epoch != 0 || clock.HeadSeq != 0 || clock.PrunedThroughSeq != 0 {
			return nil
		}
		var unused bool
		if err := tx.QueryRowContext(ctx, `SELECT NOT enabled AND coverage_fingerprint=''
			AND NOT EXISTS (SELECT 1 FROM mcp_event_subscriptions)
			AND NOT EXISTS (SELECT 1 FROM mcp_event_log)
			FROM mcp_event_clock WHERE singleton=1`).Scan(&unused); err != nil {
			return err
		}
		if !unused {
			return nil
		}
		if err := s.adoptMCPResources(ctx, tx); err != nil {
			return err
		}
		runtime, fingerprint := mcpNewRuntime(MCPEventsConfig{})
		_, err := s.adoptMCPScopeCoverage(ctx, tx, runtime, "", fingerprint)
		return err
	})
}

// Adopt once under identity -> sync/source -> Events clock fencing. Both valid
// and denied associations are durable; subsequent startup cannot reinterpret
// an old receipt after rows have been restored or numeric IDs reused.
func (s *Store) adoptMCPResources(ctx context.Context, tx *sql.Tx) error {
	var adopted bool
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM archive_metadata WHERE key=?)`), mcpResourceAdoptionKey).Scan(&adopted); err != nil {
		return err
	}
	if adopted {
		return nil
	}
	after := ""
	for {
		page, err := s.mcpLegacySubscriptionPage(ctx, tx, after)
		if err != nil {
			return err
		}
		for _, sub := range page {
			if err := s.bindMCPSubscriptionResource(ctx, tx, sub, true); err != nil {
				return err
			}
			after = sub.ID
		}
		if len(page) < 128 {
			break
		}
	}
	var afterSeq int64
	for {
		page, err := s.mcpLegacyEventPage(ctx, tx, afterSeq)
		if err != nil {
			return err
		}
		for _, event := range page {
			if err := s.bindMCPEventResource(ctx, tx, event, true); err != nil {
				return err
			}
			afterSeq = event.Seq
		}
		if len(page) < 128 {
			break
		}
	}
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key,value) VALUES (?,'1')`), mcpResourceAdoptionKey)
	return err
}

// Each bounded page closes its cursor before the adoption transaction writes.
func (s *Store) mcpLegacySubscriptionPage(ctx context.Context, tx *sql.Tx, after string) (page []*MCPSubscription, returnErr error) {
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT `+mcpSubscriptionColumns+` FROM mcp_event_subscriptions WHERE id>? ORDER BY id LIMIT 128`), after)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		sub, err := scanMCPSubscription(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, sub)
	}
	return page, rows.Err()
}

func (s *Store) mcpLegacyEventPage(ctx context.Context, tx *sql.Tx, after int64) (page []MCPEvent, returnErr error) {
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT `+mcpEventColumns+` FROM mcp_event_log WHERE seq>? ORDER BY seq LIMIT 128`), after)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		event, err := scanMCPEvent(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, event)
	}
	return page, rows.Err()
}
