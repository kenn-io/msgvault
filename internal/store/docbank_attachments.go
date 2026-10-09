package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// DocbankAttachmentRef identifies immutable uploaded bytes and their remote node.
type DocbankAttachmentRef struct {
	Endpoint   string `json:"endpoint"`
	Collection string `json:"collection"`
	NodeID     int64  `json:"node_id"`
	VersionID  string `json:"version_id"`
	BlobHash   string `json:"blob_hash"`
}

type DocbankAttachmentCandidate struct {
	ID          int64
	MessageID   int64
	SourceID    int64
	Filename    string
	MIMEType    string
	Size        int64
	ContentHash string
	StoragePath string
	State       string
	SentAt      time.Time
}

type DocbankAttachmentDelivery struct {
	ContentHash string
	Name        string
	MIMEType    string
	Size        int64
}

type DocbankAttachmentSummary struct {
	Delivered int64            `json:"delivered"`
	Pending   int64            `json:"pending"`
	Failed    int64            `json:"failed"`
	Skipped   int64            `json:"skipped"`
	Reasons   map[string]int64 `json:"reasons"`
}

// ScanDocbankAttachments reads a bounded rolling page. Completed passes wait an
// hour before revisiting old rows; new rows are always discovered immediately.
func (s *Store) ScanDocbankAttachments(ctx context.Context, destination string, limit int) ([]DocbankAttachmentCandidate, error) {
	var after int64
	var next sql.NullTime
	err := s.db.QueryRowContext(ctx, s.Rebind("SELECT after_id, next_scan_at FROM docbank_attachment_scans WHERE destination_key = ?"), destination).Scan(&after, &next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read attachment mirror scan: %w", err)
	}
	if next.Valid && !time.Now().Before(next.Time) {
		after = 0
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT a.id,a.message_id,m.source_id,COALESCE(a.filename,''),COALESCE(a.mime_type,''),COALESCE(a.size,0),COALESCE(a.content_hash,''),COALESCE(a.storage_path,''),COALESCE(a.attachment_state,''),m.sent_at
 FROM attachments a JOIN messages m ON m.id=a.message_id
 WHERE a.id > ? AND `+LiveMessagesWhere("m", true)+` ORDER BY a.id LIMIT ?`), after, limit)
	if err != nil {
		return nil, fmt.Errorf("scan attachment mirror: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []DocbankAttachmentCandidate
	for rows.Next() {
		var c DocbankAttachmentCandidate
		var sent sql.NullTime
		if err := rows.Scan(&c.ID, &c.MessageID, &c.SourceID, &c.Filename, &c.MIMEType, &c.Size, &c.ContentHash, &c.StoragePath, &c.State, &sent); err != nil {
			return nil, err
		}
		c.SentAt = sent.Time
		if c.ContentHash == "" {
			if hash, ok := casPathHash(c.StoragePath); ok {
				c.ContentHash = hash
			}
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// SaveDocbankAttachmentScan freezes upload identities before network access.
func (s *Store) SaveDocbankAttachmentScan(ctx context.Context, destination, endpoint, collection string, candidates []DocbankAttachmentCandidate, reasons []string, limit int) error {
	if len(candidates) != len(reasons) {
		return errors.New("attachment mirror reasons do not match candidates")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var after int64
		for i, c := range candidates {
			after = c.ID
			if _, err := q.Exec(`INSERT INTO docbank_attachment_occurrences(destination_key,attachment_id,content_hash,reason) VALUES(?,?,?,?) ON CONFLICT(destination_key,attachment_id) DO UPDATE SET content_hash=excluded.content_hash,reason=excluded.reason`, destination, c.ID, c.ContentHash, reasons[i]); err != nil {
				return fmt.Errorf("record attachment mirror occurrence: %w", err)
			}
			if reasons[i] != "" {
				continue
			}
			name := path.Base(strings.ReplaceAll(strings.ToValidUTF8(c.Filename, "_"), "\\", "/"))
			name = strings.Map(func(r rune) rune {
				if r < 32 || r == 127 {
					return '_'
				}
				return r
			}, name)
			name = norm.NFC.String(name)
			if name == "" || name == "." || name == "/" {
				name = "attachment"
			}
			mimeType := c.MIMEType
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			if _, err := q.Exec(`INSERT INTO docbank_attachment_deliveries(destination_key,content_hash,upload_name,mime_type,byte_length,endpoint,collection) VALUES(?,?,?,?,?,?,?) ON CONFLICT(destination_key,content_hash) DO NOTHING`, destination, c.ContentHash, c.ContentHash+"-"+name, mimeType, c.Size, endpoint, collection); err != nil {
				return fmt.Errorf("record attachment mirror delivery: %w", err)
			}
		}
		if len(candidates) == 0 {
			_, err := q.Exec(`UPDATE docbank_attachment_scans SET next_scan_at=CASE WHEN next_scan_at IS NULL OR next_scan_at<=? THEN ? ELSE next_scan_at END WHERE destination_key=?`, time.Now().UTC(), time.Now().UTC().Add(time.Hour), destination)
			return err
		}
		var next any
		if len(candidates) < limit {
			next = time.Now().UTC().Add(time.Hour)
		}
		_, err := q.Exec(`INSERT INTO docbank_attachment_scans(destination_key,after_id,next_scan_at) VALUES(?,?,?) ON CONFLICT(destination_key) DO UPDATE SET after_id=excluded.after_id,next_scan_at=CASE WHEN docbank_attachment_scans.next_scan_at>? THEN docbank_attachment_scans.next_scan_at ELSE excluded.next_scan_at END`, destination, after, next, time.Now().UTC())
		return err
	})
}

// DocbankAttachmentFilter is applied again when selecting a pending delivery.
// A narrower policy must not upload content discovered under an older policy.
type DocbankAttachmentFilter struct {
	MIMEClasses []string
	SourceIDs   []int64
	MaxBytes    int64
	After       *time.Time
}

// NextDocbankAttachment selects only content with a live, still eligible owner.
func (s *Store) NextDocbankAttachment(ctx context.Context, destination string, filter DocbankAttachmentFilter) (DocbankAttachmentDelivery, bool, error) {
	var d DocbankAttachmentDelivery
	current := ` AND COALESCE(a.size,0)=d.byte_length AND COALESCE(a.storage_path,'')<>'' AND COALESCE(a.storage_path,'') NOT LIKE 'http://%' AND COALESCE(a.storage_path,'') NOT LIKE 'https://%'`
	args := []any{destination, time.Now().UTC()}
	if filter.MaxBytes > 0 {
		current += ` AND a.size<=?`
		args = append(args, filter.MaxBytes)
	}
	if filter.After != nil {
		current += ` AND m.sent_at>=?`
		args = append(args, *filter.After)
	}
	if len(filter.SourceIDs) > 0 {
		current += ` AND m.source_id IN (?` + strings.Repeat(",?", len(filter.SourceIDs)-1) + `)`
		for _, id := range filter.SourceIDs {
			args = append(args, id)
		}
	}
	if len(filter.MIMEClasses) > 0 {
		var clauses []string
		for _, class := range filter.MIMEClasses {
			clauses = append(clauses, `LOWER(a.mime_type) LIKE ?`)
			args = append(args, class+"/%")
		}
		current += ` AND (` + strings.Join(clauses, " OR ") + `)`
	}
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT d.content_hash,d.upload_name,d.mime_type,d.byte_length FROM docbank_attachment_deliveries d
 WHERE d.destination_key=? AND d.state IN ('pending','failed') AND (d.next_action_at IS NULL OR d.next_action_at<=?)
 AND EXISTS(SELECT 1 FROM docbank_attachment_occurrences o JOIN attachments a ON a.id=o.attachment_id JOIN messages m ON m.id=a.message_id WHERE o.destination_key=d.destination_key AND o.content_hash=d.content_hash AND o.reason='' AND (a.content_hash=d.content_hash OR (COALESCE(a.content_hash,'')='' AND a.storage_path=SUBSTR(d.content_hash,1,2)||'/'||d.content_hash)) AND COALESCE(a.attachment_state,'') IN ('','stored') AND `+LiveMessagesWhere("m", true)+current+`)
 ORDER BY d.content_hash LIMIT 1`), args...).Scan(&d.ContentHash, &d.Name, &d.MIMEType, &d.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("select attachment mirror delivery: %w", err)
	}
	return d, true, nil
}

func (s *Store) CompleteDocbankAttachment(ctx context.Context, destination, hash string, ref DocbankAttachmentRef, reason string, retry bool) error {
	state := "delivered"
	var next any
	if reason != "" {
		state = "failed"
		if retry {
			next = time.Now().UTC().Add(5 * time.Minute)
		} else {
			next = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		}
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE docbank_attachment_deliveries SET state=?,reason=?,next_action_at=?,node_id=?,version_id=? WHERE destination_key=? AND content_hash=?`), state, reason, next, ref.NodeID, ref.VersionID, destination, hash)
	if err != nil {
		return fmt.Errorf("record attachment mirror result: %w", err)
	}
	return nil
}

// RetryDocbankAttachments explicitly retries failures and restarts filter discovery.
func (s *Store) RetryDocbankAttachments(ctx context.Context, destination string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`UPDATE docbank_attachment_deliveries SET next_action_at=NULL WHERE destination_key=? AND state='failed'`, destination); err != nil {
			return err
		}
		_, err := q.Exec(`DELETE FROM docbank_attachment_scans WHERE destination_key=?`, destination)
		return err
	})
}

func (s *Store) DocbankAttachmentRefs(ctx context.Context, id int64) ([]DocbankAttachmentRef, error) {
	return LoadDocbankAttachmentRefs(ctx, s.db.DB, s.Rebind, "", id)
}

// LoadDocbankAttachmentRefs is also used by query engines over the archive.
func LoadDocbankAttachmentRefs(ctx context.Context, db *sql.DB, rebind func(string) string, prefix string, id int64) ([]DocbankAttachmentRef, error) {
	query := fmt.Sprintf(`SELECT d.endpoint,d.collection,d.node_id,d.version_id,d.content_hash FROM %[1]sdocbank_attachment_deliveries d WHERE d.state='delivered' AND d.content_hash IN (SELECT CASE WHEN COALESCE(a.content_hash,'')<>'' THEN a.content_hash ELSE SUBSTR(a.storage_path,4) END FROM %[1]sattachments a WHERE a.id=? AND (COALESCE(a.content_hash,'')<>'' OR a.storage_path=SUBSTR(a.storage_path,4,2)||'/'||SUBSTR(a.storage_path,4))) ORDER BY d.destination_key`, prefix)
	rows, err := db.QueryContext(ctx, rebind(query), id)
	if err != nil {
		return nil, fmt.Errorf("read attachment mirror receipts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []DocbankAttachmentRef
	for rows.Next() {
		var r DocbankAttachmentRef
		if err := rows.Scan(&r.Endpoint, &r.Collection, &r.NodeID, &r.VersionID, &r.BlobHash); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

func (s *Store) DocbankAttachmentStatus(ctx context.Context, destination string) (DocbankAttachmentSummary, error) {
	result := DocbankAttachmentSummary{Reasons: make(map[string]int64)}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT CASE WHEN o.reason<>'' THEN 'skipped' ELSE COALESCE(d.state,'pending') END,CASE WHEN o.reason<>'' THEN o.reason ELSE COALESCE(d.reason,'') END,COUNT(*) FROM docbank_attachment_occurrences o JOIN attachments a ON a.id=o.attachment_id JOIN messages m ON m.id=a.message_id LEFT JOIN docbank_attachment_deliveries d ON d.destination_key=o.destination_key AND d.content_hash=o.content_hash WHERE o.destination_key=? AND `+LiveMessagesWhere("m", true)+` GROUP BY CASE WHEN o.reason<>'' THEN 'skipped' ELSE COALESCE(d.state,'pending') END,CASE WHEN o.reason<>'' THEN o.reason ELSE COALESCE(d.reason,'') END`), destination)
	if err != nil {
		return result, fmt.Errorf("attachment mirror status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state, reason string
		var count int64
		if err := rows.Scan(&state, &reason, &count); err != nil {
			return result, err
		}
		switch state {
		case "delivered":
			result.Delivered += count
		case "pending":
			result.Pending += count
		case "failed":
			result.Failed += count
		case "skipped":
			result.Skipped += count
		}
		if reason != "" {
			result.Reasons[reason] += count
		}
	}
	return result, rows.Err()
}
