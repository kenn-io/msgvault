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

// VirtualAccount is an archived account scope. It never owns credentials,
// cursors, labels, or a sync schedule. SourceID remains the physical source.
type VirtualAccount struct {
	Key                string `json:"key"`
	SourceID           int64  `json:"source_id"`
	AccountAddress     string `json:"account_address,omitempty"`
	Group              string `json:"group,omitempty"`
	Unattributed       bool   `json:"unattributed,omitempty"`
	MessageCount       int64  `json:"message_count"`
	SourceDeletedCount int64  `json:"source_deleted_count"`
	PendingCount       int64  `json:"pending_count,omitempty"`
}

func VirtualIdentityKey(sourceID int64, address string) string {
	return "identity:" + strconv.FormatInt(sourceID, 10) + ":" + base64.RawURLEncoding.EncodeToString([]byte(address))
}

// ParseVirtualAccountKey decodes stable identity/bucket keys. Group keys carry
// their own source binding through membership; callers intersect physical IDs.
func ParseVirtualAccountKey(key string) (int64, string, bool, error) {
	parts := strings.Split(key, ":")
	if len(parts) == 2 && parts[0] == "unattributed" {
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return 0, "", false, errors.New("invalid virtual account key")
		}
		return id, "", true, nil
	}
	if len(parts) != 3 || (parts[0] != "identity" && parts[0] != "group") {
		return 0, "", false, errors.New("invalid virtual account key")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, "", false, errors.New("invalid virtual account source")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(data) == 0 {
		return 0, "", false, errors.New("invalid virtual account address")
	}
	return id, string(data), false, nil
}

func (s *Store) ListVirtualAccountsContext(ctx context.Context) (map[int64][]VirtualAccount, error) {
	return ReadVirtualAccountsContext(ctx, s.DB(), s.Rebind)
}

// ReadVirtualAccountsContext is the Store-owned read projection shared with
// query engines. Grouping happens in SQL so thousands of masks produce one row.
func ReadVirtualAccountsContext(ctx context.Context, db *sql.DB, rebind func(string) string) (map[int64][]VirtualAccount, error) {
	stmt := `WITH candidates AS (
 SELECT ai.source_id,ai.address_key AS address,'' AS group_key FROM account_identities ai
 WHERE EXISTS (SELECT 1 FROM sources src WHERE src.id=ai.source_id AND src.source_type IN ('gmail','imap','o365','msmail','mbox','hey','apple-mail','pst','eml','maildir','gcal')) AND ai.address_key LIKE '%@%' AND NOT EXISTS (SELECT 1 FROM account_identity_group_memberships g WHERE g.source_id=ai.source_id AND g.address_key=ai.address_key AND g.address_key<>'')
 UNION ALL
 SELECT source_id,'' AS address,group_key FROM account_identity_group_memberships GROUP BY source_id,group_key
 UNION ALL
 SELECT id,'' AS address,'' AS group_key FROM sources WHERE source_type IN ('gmail','imap','o365','msmail','mbox','hey','apple-mail','pst','eml','maildir','gcal')
 ), address_counts AS (
 SELECT m.source_id,COALESCE(m.account_address,'') AS address,
 SUM(CASE WHEN m.deleted_from_source_at IS NULL THEN 1 ELSE 0 END) AS live,
 SUM(CASE WHEN m.deleted_from_source_at IS NOT NULL THEN 1 ELSE 0 END) AS source_deleted,
 SUM(CASE WHEN m.account_attribution_basis='not-derived' THEN 1 ELSE 0 END) AS pending
 FROM messages m WHERE m.deleted_at IS NULL AND (m.message_type IN ('email','calendar_event') OR m.message_type IS NULL OR m.message_type='')
 GROUP BY m.source_id,m.account_address
 ), grouped_addresses AS (
 SELECT a.source_id,COALESCE((SELECT MIN(g.group_key) FROM account_identity_group_memberships g WHERE g.source_id=a.source_id AND g.address_key=a.address AND g.address_key<>''),a.address) AS bucket,
 a.live,a.source_deleted,a.pending FROM address_counts a
 ), counts AS (
 SELECT source_id,bucket,SUM(live) AS live,SUM(source_deleted) AS source_deleted,SUM(pending) AS pending
 FROM grouped_addresses GROUP BY source_id,bucket
 )
 SELECT c.source_id,c.address,c.group_key,COALESCE(n.live,0),COALESCE(n.source_deleted,0),COALESCE(n.pending,0)
 FROM candidates c LEFT JOIN counts n ON n.source_id=c.source_id AND n.bucket=CASE WHEN c.group_key<>'' THEN c.group_key ELSE c.address END
 ORDER BY c.source_id,c.group_key,c.address`
	rows, err := db.QueryContext(ctx, rebind(stmt))
	if err != nil {
		return nil, fmt.Errorf("list virtual accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64][]VirtualAccount)
	for rows.Next() {
		var v VirtualAccount
		if err = rows.Scan(&v.SourceID, &v.AccountAddress, &v.Group, &v.MessageCount, &v.SourceDeletedCount, &v.PendingCount); err != nil {
			return nil, err
		}
		v.Unattributed = v.AccountAddress == "" && v.Group == ""
		switch {
		case v.Group != "":
			v.Key = "group:" + strconv.FormatInt(v.SourceID, 10) + ":" + base64.RawURLEncoding.EncodeToString([]byte(v.Group))
		case v.Unattributed:
			v.Key = "unattributed:" + strconv.FormatInt(v.SourceID, 10)
		default:
			v.Key = VirtualIdentityKey(v.SourceID, v.AccountAddress)
		}
		out[v.SourceID] = append(out[v.SourceID], v)
	}
	return out, rows.Err()
}
