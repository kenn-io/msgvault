package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/agentgrant"
)

func (s *Store) SaveAgentGrant(ctx context.Context, r agentgrant.Record) error {
	digest, err := hex.DecodeString(r.Digest)
	if err != nil || len(digest) != 32 {
		return errors.New("agent grant digest must be SHA-256")
	}
	data, err := json.Marshal(r.Grant)
	if err != nil {
		return fmt.Errorf("encode agent grant: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_grants (id,secret_hash,record) VALUES (?,?,?)`, r.Grant.ID, r.Digest, string(data))
	if err != nil {
		return fmt.Errorf("save agent grant: %w", err)
	}
	return nil
}
func (s *Store) FindAgentGrant(ctx context.Context, digest string) (agentgrant.Record, bool, error) {
	var record string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM agent_grants WHERE secret_hash=?`, digest).Scan(&record)
	if errors.Is(err, sql.ErrNoRows) {
		return agentgrant.Record{}, false, nil
	}
	if err != nil {
		return agentgrant.Record{}, false, fmt.Errorf("find agent grant: %w", err)
	}
	r := agentgrant.Record{Digest: digest}
	err = json.Unmarshal([]byte(record), &r.Grant)
	if err != nil {
		return agentgrant.Record{}, false, fmt.Errorf("decode agent grant: %w", err)
	}
	return r, true, nil
}
func (s *Store) ListAgentGrants(ctx context.Context) ([]agentgrant.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT secret_hash,record FROM agent_grants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list agent grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]agentgrant.Record, 0)
	for rows.Next() {
		var r agentgrant.Record
		var record string
		if err := rows.Scan(&r.Digest, &record); err != nil {
			return nil, fmt.Errorf("scan agent grant: %w", err)
		}
		if err := json.Unmarshal([]byte(record), &r.Grant); err != nil {
			return nil, fmt.Errorf("decode agent grant: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) DeleteAgentGrant(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_grants WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("revoke agent grant: %w", err)
	}
	return nil
}
