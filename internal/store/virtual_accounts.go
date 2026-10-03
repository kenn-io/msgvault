package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// VirtualAccount is one selectable slice of a source's email and calendar
// rows: a confirmed identity, or the rows no confirmed identity claims. It
// owns no credentials, cursors or schedule; SourceID stays the physical source.
type VirtualAccount struct {
	Key                string `json:"key"`
	SourceID           int64  `json:"source_id"`
	AccountAddress     string `json:"account_address,omitempty"`
	Unattributed       bool   `json:"unattributed,omitempty"`
	MessageCount       int64  `json:"message_count"`
	SourceDeletedCount int64  `json:"source_deleted_count"`
	// PendingCount counts unattributed rows still waiting for the repair pass.
	PendingCount int64 `json:"pending_count,omitempty"`
}

// VirtualIdentityKey is the stable key of a source's confirmed identity.
func VirtualIdentityKey(sourceID int64, address string) string {
	return "identity:" + strconv.FormatInt(sourceID, 10) + ":" + base64.RawURLEncoding.EncodeToString([]byte(address))
}

// VirtualUnattributedKey is the stable key of a source's unattributed rows.
func VirtualUnattributedKey(sourceID int64) string {
	return "unattributed:" + strconv.FormatInt(sourceID, 10)
}

// IsVirtualAccountKey reports whether value has a virtual account key prefix.
func IsVirtualAccountKey(value string) bool {
	return strings.HasPrefix(value, "identity:") || strings.HasPrefix(value, "unattributed:")
}

// ParseVirtualAccountKey decodes a key from VirtualIdentityKey or
// VirtualUnattributedKey into its source, address and unattributed flag.
func ParseVirtualAccountKey(key string) (sourceID int64, address string, unattributed bool, err error) {
	parts := strings.Split(key, ":")
	switch {
	case len(parts) == 2 && parts[0] == "unattributed":
		unattributed = true
	case len(parts) == 3 && parts[0] == "identity":
	default:
		return 0, "", false, errors.New("invalid virtual account key")
	}
	sourceID, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || sourceID <= 0 {
		return 0, "", false, errors.New("invalid virtual account source")
	}
	if unattributed {
		return sourceID, "", true, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(data) == 0 {
		return 0, "", false, errors.New("invalid virtual account address")
	}
	return sourceID, string(data), false, nil
}

// ListVirtualAccountsContext reads the virtual account catalog.
func (s *Store) ListVirtualAccountsContext(ctx context.Context) (map[int64][]VirtualAccount, error) {
	return ReadVirtualAccountsContext(ctx, s.db.DB, s.Rebind)
}

// ReadVirtualAccountsContext lists, per source with email or calendar rows,
// one child per confirmed identity and one for unattributed rows. Counts
// group in SQL, so the read is one pass over messages.
func ReadVirtualAccountsContext(
	ctx context.Context, db *sql.DB, rebind func(string) string,
) (map[int64][]VirtualAccount, error) {
	stmt := fmt.Sprintf(`
		WITH counts AS (
			SELECT m.source_id, m.account_address AS address,
				SUM(CASE WHEN %s THEN 1 ELSE 0 END) AS live,
				SUM(CASE WHEN %s THEN 1 ELSE 0 END) AS source_deleted,
				SUM(CASE WHEN m.account_path IS NULL AND m.deleted_at IS NULL THEN 1 ELSE 0 END) AS pending
			FROM messages m
			WHERE COALESCE(m.message_type, '') IN ('', 'email', 'calendar_event')
			GROUP BY m.source_id, m.account_address
		)
		SELECT ai.source_id, ai.address_key, COALESCE(c.live, 0), COALESCE(c.source_deleted, 0), 0
		FROM account_identities ai
		LEFT JOIN counts c ON c.source_id = ai.source_id AND c.address = ai.address_key
		WHERE ai.address_key LIKE '%%@%%'
		  AND EXISTS (SELECT 1 FROM counts e WHERE e.source_id = ai.source_id)
		UNION ALL
		SELECT c.source_id, '', c.live, c.source_deleted, c.pending
		FROM counts c
		WHERE c.address IS NULL
		ORDER BY 1, 2`,
		LiveMessagesWhere("m", true), SourceDeletedMessagesWhere("m"))
	if rebind != nil {
		stmt = rebind(stmt)
	}
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("list virtual accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64][]VirtualAccount)
	for rows.Next() {
		var v VirtualAccount
		if err := rows.Scan(&v.SourceID, &v.AccountAddress, &v.MessageCount, &v.SourceDeletedCount, &v.PendingCount); err != nil {
			return nil, fmt.Errorf("scan virtual account: %w", err)
		}
		if v.AccountAddress == "" {
			v.Unattributed = true
			v.Key = VirtualUnattributedKey(v.SourceID)
		} else {
			v.Key = VirtualIdentityKey(v.SourceID, v.AccountAddress)
		}
		out[v.SourceID] = append(out[v.SourceID], v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list virtual accounts: %w", err)
	}
	return out, nil
}
