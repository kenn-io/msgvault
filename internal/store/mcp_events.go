package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

const mcpEventsCatalogKey = "mcpevents:v1:subscriptions"

func MCPEventsPositionKey(id string) string { return "mcpevents:v1:pos:" + id }

func (s *Store) LoadMCPEventsCatalog(ctx context.Context) ([]byte, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, mcpEventsCatalogKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load events catalog: %w", err)
	}
	return []byte(raw), nil
}

// SwapMCPEventsCatalog updates the catalog and creates or deletes a position atomically.
func (s *Store) SwapMCPEventsCatalog(ctx context.Context, before, after []byte, createID string, initial *WakeupState, deleteID string) (bool, error) {
	var swapped bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var err error
		swapped, err = casArchiveMetadata(q, mcpEventsCatalogKey, before, after)
		if err != nil {
			return err
		}
		if !swapped {
			return nil
		}
		if createID != "" {
			if initial == nil {
				return errors.New("missing initial events position")
			}
			raw, err := json.Marshal(initial)
			if err != nil {
				return err
			}
			if _, err = q.Exec(`INSERT INTO archive_metadata (key,value) VALUES (?,?)`, MCPEventsPositionKey(createID), string(raw)); err != nil {
				return err
			}
		}
		if deleteID != "" {
			_, err = q.Exec(`DELETE FROM archive_metadata WHERE key=?`, MCPEventsPositionKey(deleteID))
			return err
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("swap events catalog: %w", err)
	}
	return swapped, nil
}
