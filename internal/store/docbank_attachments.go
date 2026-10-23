package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kit/packstore"
)

type DocbankAttachmentRef struct {
	Endpoint  string `json:"endpoint"`
	Folder    string `json:"folder"`
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	BlobHash  string `json:"blob_hash"`
}

type DocbankAttachmentDelivery struct {
	AttachmentID int64
	ContentHash  string
	Name         string
	MIMEType     string
	Attempts     int
}

type DocbankAttachmentSummary struct {
	Sending   int64            `json:"sending"`
	Delivered int64            `json:"delivered"`
	Remaining int64            `json:"remaining"`
	Failed    int64            `json:"failed"`
	Reasons   map[string]int64 `json:"reasons"`
}

// AttachmentContentHash recovers a missing hash from a trusted CAS path.
func AttachmentContentHash(contentHash, storagePath string) string {
	if contentHash == "" {
		contentHash, _ = casPathHash(storagePath)
	}
	return contentHash
}

// AttachmentReceiptHash keeps hashless pending rows out of receipt lookup.
func AttachmentReceiptHash(contentHash, storagePath, sourceAttachmentID string) string {
	_, receiptHash := AttachmentReadHashes(contentHash, storagePath, sourceAttachmentID)
	return receiptHash
}

// AttachmentReadHashes recovers a CAS hash once and restricts alias receipts to trusted providers.
func AttachmentReadHashes(contentHash, storagePath, sourceAttachmentID string) (string, string) {
	recoveredHash := AttachmentContentHash(contentHash, storagePath)
	if IsURLStoragePath(storagePath) {
		return recoveredHash, ""
	}
	trustedAlias := slices.ContainsFunc(docbankAliasProviders, func(provider string) bool {
		return strings.HasPrefix(sourceAttachmentID, provider+":")
	})
	if contentHash == "" && !trustedAlias {
		return recoveredHash, ""
	}
	return recoveredHash, recoveredHash
}

// IsURLStoragePath reports a remote storage path, matching the scheme case-insensitively.
func IsURLStoragePath(storagePath string) bool {
	lowerPath := strings.ToLower(storagePath)
	return strings.HasPrefix(lowerPath, "http://") || strings.HasPrefix(lowerPath, "https://")
}

type DocbankAttachmentPage struct {
	Files  []DocbankAttachmentDelivery
	Before BeeperMediaScan
	After  BeeperMediaScan
	More   bool
}

var ErrDocbankAttachmentCursorChanged = errors.New("attachment mirror cursor changed concurrently")

// docbankAttachmentPageSize bounds the attachment IDs one discovery page examines.
const docbankAttachmentPageSize = 500

// docbankReceiptChunk bounds the hashes in one receipt query below backend variable limits.
const docbankReceiptChunk = 512

func docbankAttachmentEligibleWhere() string {
	nonHex := `SUBSTR(a.storage_path,4)`
	for _, digit := range "0123456789abcdef" {
		nonHex = `REPLACE(` + nonHex + `,'` + string(digit) + `','')`
	}
	hashlessAlias := `(` + hashlessProviderAliasBytes + `
		AND LENGTH(a.storage_path) = 67
		AND SUBSTR(a.storage_path,1,3) = SUBSTR(a.storage_path,4,2) || '/'
		AND ` + nonHex + ` = '')`
	return `COALESCE(a.attachment_state,'') IN ('','stored')
		AND LOWER(a.storage_path) NOT LIKE 'http://%'
		AND LOWER(a.storage_path) NOT LIKE 'https://%'
		AND (` + attachmentHashedBytes + ` OR ` + hashlessAlias + `)`
}

// docbankAttachmentSourceExists matches an eligible attachment for hash, a SQL expression.
func docbankAttachmentSourceExists(hash string) string {
	eligible := ` AND ` + docbankAttachmentEligibleWhere()
	hashMatch, pathMatch := docbankAttachmentMatch("a", hash)
	return `(EXISTS (
			SELECT 1 FROM attachments a
			WHERE ` + hashMatch + eligible + `)
		OR EXISTS (
			SELECT 1 FROM attachments a
			WHERE ` + pathMatch + eligible + `))`
}

// docbankAttachmentMatch keeps hash and hashless CAS lookups separately indexable.
func docbankAttachmentMatch(alias, hash string) (string, string) {
	// LOWER on both sides lets SQLite use the expression index for a correlated hash.
	return `LOWER(` + alias + `.content_hash) = LOWER(` + hash + `)`,
		alias + `.storage_path = SUBSTR(` + hash + `,1,2) || '/' || ` + hash + `
			  AND COALESCE(` + alias + `.content_hash,'') = ''`
}

// DocbankAttachmentStoragePaths returns local candidates for a mirror source hash.
func (s *Store) DocbankAttachmentStoragePaths(ctx context.Context, hash string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT DISTINCT a.storage_path FROM attachments a
		WHERE `+docbankAttachmentEligibleWhere()+`
		  AND (LOWER(a.content_hash) = ?
		    OR (COALESCE(a.content_hash,'') = '' AND a.storage_path = SUBSTR(?,1,2) || '/' || ?))
		ORDER BY a.storage_path`), hash, hash, hash)
	if err != nil {
		return nil, fmt.Errorf("read attachment mirror storage paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan attachment mirror storage path: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

type docbankAttachmentPackResolver struct{ store *Store }

// NewDocbankAttachmentPackResolver admits trusted mirror aliases without changing archive membership.
func NewDocbankAttachmentPackResolver(s *Store) packstore.Resolver {
	return docbankAttachmentPackResolver{store: s}
}

func (r docbankAttachmentPackResolver) Resolve(ctx context.Context, hash packstore.Hash) (packstore.Location, error) {
	var exists bool
	err := r.store.db.QueryRowContext(ctx, r.store.Rebind(`SELECT `+docbankAttachmentSourceExists("?")),
		hash.String(), hash.String(), hash.String()).Scan(&exists)
	if err != nil || !exists {
		return packstore.Location{}, err
	}
	entry, err := r.store.GetAttachmentPackEntry(hash.String())
	if err != nil || entry == nil {
		return packstore.Location{}, err
	}
	indexed, err := fromStorePackEntry(*entry)
	if err != nil {
		return packstore.Location{}, err
	}
	return packstore.Location{Member: true, Pack: &indexed}, nil
}

// LoadDocbankAttachmentScan persists a generation so backfill rejects stale pages.
func (s *Store) LoadDocbankAttachmentScan(ctx context.Context, destination string) (BeeperMediaScan, error) {
	return s.loadBeeperMediaScan(ctx, destination, true)
}

// AdvanceDocbankAttachmentScan swaps an existing checkpoint without undoing a reset.
func (s *Store) AdvanceDocbankAttachmentScan(
	ctx context.Context, destination string, before, after BeeperMediaScan,
) (bool, error) {
	return s.advanceBeeperMediaScan(ctx, destination, before, after, false)
}

// NextDocbankAttachmentPage reads the next discovery page of files without a delivery row.
func (s *Store) NextDocbankAttachmentPage(ctx context.Context, destination string) (DocbankAttachmentPage, error) {
	var page DocbankAttachmentPage
	scan, err := s.docbankAttachmentPassScan(ctx, destination)
	if err != nil {
		return page, err
	}
	page.Before, page.After = scan, scan
	if page.After.PassHighWater == 0 {
		page.After.PassHighWater, err = s.BeeperMediaAttachmentHighWater(ctx)
		if err != nil {
			return page, err
		}
	}
	var last sql.NullInt64
	err = s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT MAX(id) FROM (
			SELECT id FROM attachments WHERE id > ? AND id <= ? ORDER BY id LIMIT ?
		) page`), scan.AfterAttachmentID, page.After.PassHighWater, docbankAttachmentPageSize).Scan(&last)
	if err != nil {
		return page, fmt.Errorf("read attachment mirror page: %w", err)
	}
	if !last.Valid {
		page.After.PassHighWater = 0
		now := time.Now().UTC()
		page.More = !scan.NextFullScanAt.IsZero() && !now.Before(scan.NextFullScanAt)
		if scan.NextFullScanAt.IsZero() || page.More {
			page.After.NextFullScanAt = now.Add(24 * time.Hour)
		}
		if page.More {
			page.After.AfterAttachmentID = 0
		}
		return page, nil
	}
	page.More = true
	page.After.AfterAttachmentID = last.Int64
	page.Files, err = s.docbankAttachmentCandidates(ctx, scan.AfterAttachmentID, last.Int64)
	if err != nil {
		return page, err
	}
	page.Files, err = s.withoutDocbankDeliveries(ctx, destination, page.Files)
	return page, err
}

// docbankAttachmentPassScan loads the checkpoint, restarting it for a new journal registration.
func (s *Store) docbankAttachmentPassScan(ctx context.Context, destination string) (BeeperMediaScan, error) {
	key := DocbankAttachmentConsumerKey(destination)
	consumer, err := s.GetAttachmentChangeConsumer(ctx, key)
	var created bool
	if errors.Is(err, ErrAttachmentChangeConsumerMissing) {
		consumer, created, err = s.RegisterAttachmentChangeConsumer(ctx, key)
	}
	if err != nil {
		return BeeperMediaScan{}, err
	}
	scan, err := s.LoadDocbankAttachmentScan(ctx, destination)
	if err != nil {
		return BeeperMediaScan{}, err
	}
	if consumer.ReconciliationComplete || (!created && scan.BaselineSequence == consumer.BaselineSequence) {
		return scan, nil
	}
	reset := BeeperMediaScan{
		DestinationKey: destination, Generation: scan.Generation, BaselineSequence: consumer.BaselineSequence,
	}
	swapped, err := s.AdvanceDocbankAttachmentScan(ctx, destination, scan, reset)
	if err != nil {
		return BeeperMediaScan{}, err
	}
	if !swapped {
		return BeeperMediaScan{}, ErrDocbankAttachmentCursorChanged
	}
	return reset, nil
}

// docbankAttachmentCandidates returns the first eligible file per content hash in an ID range.
func (s *Store) docbankAttachmentCandidates(ctx context.Context, afterID, lastID int64) ([]DocbankAttachmentDelivery, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT a.id, COALESCE(a.content_hash,''), COALESCE(a.storage_path,''),
		       COALESCE(a.filename,''), COALESCE(a.mime_type,'')
		FROM attachments a
		WHERE a.id > ? AND a.id <= ? AND `+docbankAttachmentEligibleWhere()+`
		ORDER BY a.id`), afterID, lastID)
	if err != nil {
		return nil, fmt.Errorf("read attachment mirror candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]bool)
	var files []DocbankAttachmentDelivery
	for rows.Next() {
		var d DocbankAttachmentDelivery
		var path string
		if err := rows.Scan(&d.AttachmentID, &d.ContentHash, &path, &d.Name, &d.MIMEType); err != nil {
			return nil, fmt.Errorf("scan attachment mirror candidate: %w", err)
		}
		d.ContentHash = strings.ToLower(AttachmentContentHash(d.ContentHash, path))
		if !seen[d.ContentHash] {
			seen[d.ContentHash] = true
			files = append(files, d)
		}
	}
	return files, rows.Err()
}

// withoutDocbankDeliveries drops files that already have an outcome other than a missing source.
func (s *Store) withoutDocbankDeliveries(
	ctx context.Context, destination string, files []DocbankAttachmentDelivery,
) ([]DocbankAttachmentDelivery, error) {
	known := make(map[string]bool)
	hashes := make([]any, 0, len(files))
	for _, d := range files {
		hashes = append(hashes, d.ContentHash)
	}
	for chunk := range slices.Chunk(hashes, docbankReceiptChunk) {
		err := func() error {
			rows, err := s.db.QueryContext(ctx, s.Rebind(`
				SELECT content_hash FROM docbank_attachment_deliveries
				WHERE destination_key = ?
				  AND NOT (state = 'failed' AND reason = 'source_unavailable')
				  AND content_hash IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`),
				append([]any{destination}, chunk...)...)
			if err != nil {
				return fmt.Errorf("read attachment mirror outcomes: %w", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var hash string
				if err := rows.Scan(&hash); err != nil {
					return fmt.Errorf("scan attachment mirror outcome: %w", err)
				}
				known[hash] = true
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(files, func(d DocbankAttachmentDelivery) bool { return known[d.ContentHash] }), nil
}

// FinishDocbankAttachmentPage commits a page and completes reconciliation after the final page.
func (s *Store) FinishDocbankAttachmentPage(ctx context.Context, destination string, page DocbankAttachmentPage) error {
	if page.Before != page.After {
		swapped, err := s.AdvanceDocbankAttachmentScan(ctx, destination, page.Before, page.After)
		if err != nil {
			return err
		}
		if !swapped {
			return ErrDocbankAttachmentCursorChanged
		}
	}
	if page.More {
		return nil
	}
	consumer, err := s.GetAttachmentChangeConsumer(ctx, DocbankAttachmentConsumerKey(destination))
	if err != nil {
		return err
	}
	if consumer.ReconciliationComplete {
		return nil
	}
	return s.CompleteAttachmentChangeReconciliation(ctx, consumer.ConsumerKey, page.After.BaselineSequence)
}

// SendingDocbankAttachments fails intents whose source vanished and returns due intents.
func (s *Store) SendingDocbankAttachments(ctx context.Context, destination string) ([]DocbankAttachmentDelivery, error) {
	_, err := s.db.ExecContext(ctx, s.Rebind(`
		UPDATE docbank_attachment_deliveries AS d
		SET state = 'failed', reason = 'source_unavailable'
		WHERE destination_key = ? AND state = 'sending'
		  AND NOT `+docbankAttachmentSourceExists("d.content_hash")), destination)
	if err != nil {
		return nil, fmt.Errorf("fail attachment mirror intents without a source: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT content_hash, upload_name, mime_type, attempts
		FROM docbank_attachment_deliveries
		WHERE destination_key = ? AND state = 'sending'
		  AND (next_action_at IS NULL OR next_action_at <= ?)
		ORDER BY attempts, content_hash
		LIMIT ?`), destination, s.timestampValue(time.Now().UTC()), docbankAttachmentPageSize)
	if err != nil {
		return nil, fmt.Errorf("read attachment mirror intents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var files []DocbankAttachmentDelivery
	for rows.Next() {
		var d DocbankAttachmentDelivery
		if err := rows.Scan(&d.ContentHash, &d.Name, &d.MIMEType, &d.Attempts); err != nil {
			return nil, fmt.Errorf("scan attachment mirror intent: %w", err)
		}
		files = append(files, d)
	}
	return files, rows.Err()
}

// SaveDocbankAttachmentIntent claims a due intent for one upload attempt.
// It returns sql.ErrNoRows when the file has no source or is not due.
func (s *Store) SaveDocbankAttachmentIntent(
	ctx context.Context, destination string, d DocbankAttachmentDelivery,
) (DocbankAttachmentDelivery, error) {
	d.ContentHash = strings.ToLower(d.ContentHash)
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		INSERT INTO docbank_attachment_deliveries
			(destination_key, content_hash, upload_name, mime_type, state, endpoint, folder, attempts)
		SELECT ?, ?, ?, ?, 'sending', '', '', 1
		WHERE `+docbankAttachmentSourceExists("?")+`
		ON CONFLICT (destination_key, content_hash) DO UPDATE SET
			state = 'sending', reason = '', next_action_at = NULL,
			attempts = CASE WHEN docbank_attachment_deliveries.state = 'failed' THEN 1
			                ELSE docbank_attachment_deliveries.attempts + 1 END
		WHERE (docbank_attachment_deliveries.state = 'sending'
		       AND (docbank_attachment_deliveries.next_action_at IS NULL
		            OR docbank_attachment_deliveries.next_action_at <= ?))
		   OR (docbank_attachment_deliveries.state = 'failed'
		       AND docbank_attachment_deliveries.reason = 'source_unavailable')
		RETURNING upload_name, mime_type, attempts`),
		destination, d.ContentHash, d.Name, d.MIMEType, d.ContentHash, d.ContentHash, d.ContentHash,
		s.timestampValue(time.Now().UTC()),
	).Scan(&d.Name, &d.MIMEType, &d.Attempts)
	return d, err
}

// QueueDocbankAttachmentIntents records discovered files as sending intents in one transaction.
// Existing outcomes stay unless an earlier attempt found no source.
func (s *Store) QueueDocbankAttachmentIntents(
	ctx context.Context, destination string, files ...DocbankAttachmentDelivery,
) error {
	if len(files) == 0 {
		return nil
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		for _, d := range files {
			if _, err := q.Exec(`
				INSERT INTO docbank_attachment_deliveries
					(destination_key, content_hash, upload_name, mime_type, state, endpoint, folder, attempts)
				VALUES (?, ?, ?, ?, 'sending', '', '', 0)
				ON CONFLICT (destination_key, content_hash) DO UPDATE SET
					state = 'sending', reason = '', attempts = 0, next_action_at = NULL
				WHERE docbank_attachment_deliveries.state = 'failed'
				  AND docbank_attachment_deliveries.reason = 'source_unavailable'`,
				destination, strings.ToLower(d.ContentHash), d.Name, d.MIMEType); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("queue attachment mirror intents: %w", err)
	}
	return nil
}

// RecordDocbankAttachment stores a receipt, or a terminal failure when reason is set.
func (s *Store) RecordDocbankAttachment(
	ctx context.Context, destination string, d DocbankAttachmentDelivery, ref DocbankAttachmentRef, reason string,
) error {
	state := "delivered"
	if reason != "" {
		state = "failed"
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`
		INSERT INTO docbank_attachment_deliveries
			(destination_key, content_hash, upload_name, mime_type, state, reason,
			 node_id, version_id, endpoint, folder)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (destination_key, content_hash) DO UPDATE SET
			state = excluded.state, reason = excluded.reason, node_id = excluded.node_id,
			version_id = excluded.version_id, endpoint = excluded.endpoint, folder = excluded.folder`),
		destination, strings.ToLower(d.ContentHash), d.Name, d.MIMEType, state, reason,
		ref.NodeID, ref.VersionID, ref.Endpoint, ref.Folder)
	if err != nil {
		return fmt.Errorf("record attachment mirror result: %w", err)
	}
	return nil
}

// RetryDocbankAttachments requeues failures and restarts discovery in one transaction.
func (s *Store) RetryDocbankAttachments(ctx context.Context, destination string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`
			UPDATE docbank_attachment_deliveries
			SET state = 'sending', reason = '', attempts = 0, next_action_at = NULL
			WHERE destination_key = ? AND state = 'failed'`, destination); err != nil {
			return fmt.Errorf("requeue failed attachment mirror deliveries: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM docbank_attachment_processing WHERE destination_key = ? AND (job_id = '' OR outcome IN ('requested', 'consent', 'stale_authority'))`, destination); err != nil {
			return fmt.Errorf("retry attachment processing: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM archive_metadata WHERE key = ?`, beeperMediaScanKey(destination)); err != nil {
			return fmt.Errorf("reset attachment mirror discovery: %w", err)
		}
		return nil
	})
}

// RecordDocbankAttachmentRetry delays a sending intent after a temporary failure.
func (s *Store) RecordDocbankAttachmentRetry(
	ctx context.Context, destination string, d DocbankAttachmentDelivery, reason string,
) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`
		UPDATE docbank_attachment_deliveries SET reason = ?, next_action_at = ?
		WHERE destination_key = ? AND content_hash = ? AND state = 'sending'`),
		reason, s.timestampValue(time.Now().UTC().Add(BeeperMediaRetryDelay)), destination, d.ContentHash)
	if err != nil {
		return fmt.Errorf("record attachment mirror retry: %w", err)
	}
	return nil
}

// LoadDocbankAttachmentRefs returns delivered receipts keyed by lowercase hash.
// Only destinations with a registered journal consumer, the active mirror, contribute.
func LoadDocbankAttachmentRefs(
	ctx context.Context, db SQLReader, rebind func(string) string, prefix string, hashes []string,
) (map[string][]DocbankAttachmentRef, error) {
	refs := make(map[string][]DocbankAttachmentRef)
	seen := make(map[string]bool, len(hashes))
	var unique []any
	for _, hash := range hashes {
		if hash = strings.ToLower(hash); hash != "" && !seen[hash] {
			seen[hash] = true
			unique = append(unique, hash)
		}
	}
	for chunk := range slices.Chunk(unique, docbankReceiptChunk) {
		query := fmt.Sprintf(`
			SELECT endpoint, folder, node_id, version_id, content_hash
			FROM %[1]sdocbank_attachment_deliveries d
			WHERE state = 'delivered'
			  AND EXISTS (
				SELECT 1 FROM %[1]sattachment_change_consumers c
				WHERE c.consumer_key = 'docbank-mirror:' || d.destination_key)
			  AND content_hash IN (?%[2]s)
			ORDER BY destination_key`, prefix, strings.Repeat(",?", len(chunk)-1))
		err := func() error {
			rows, err := db.QueryContext(ctx, rebind(query), chunk...)
			if err != nil {
				return fmt.Errorf("read attachment mirror receipts: %w", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var r DocbankAttachmentRef
				if err := rows.Scan(&r.Endpoint, &r.Folder, &r.NodeID, &r.VersionID, &r.BlobHash); err != nil {
					return fmt.Errorf("scan attachment mirror receipt: %w", err)
				}
				refs[r.BlobHash] = append(refs[r.BlobHash], r)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// DocbankAttachmentStatus counts outcomes and distinct eligible files without a delivery row.
func (s *Store) DocbankAttachmentStatus(ctx context.Context, destination string) (DocbankAttachmentSummary, error) {
	result := DocbankAttachmentSummary{Reasons: make(map[string]int64)}
	fileHash := `LOWER(COALESCE(NULLIF(a.content_hash,''), SUBSTR(a.storage_path,4)))`
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT COUNT(DISTINCT `+fileHash+`) FROM attachments a
		WHERE `+docbankAttachmentEligibleWhere()+`
		  AND NOT EXISTS (
			SELECT 1 FROM docbank_attachment_deliveries d
			WHERE d.destination_key = ? AND d.content_hash = `+fileHash+`)`), destination).Scan(&result.Remaining)
	if err != nil {
		return result, fmt.Errorf("count remaining attachment mirror files: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT state,
		       CASE WHEN state = 'sending' AND attempts > 0 AND reason = '' THEN 'retrying'
		            ELSE reason END AS status_reason,
		       COUNT(*)
		FROM docbank_attachment_deliveries d
		WHERE destination_key = ?
		  AND (state <> 'sending' OR `+docbankAttachmentSourceExists("d.content_hash")+`)
		GROUP BY state, status_reason`), destination)
	if err != nil {
		return result, fmt.Errorf("attachment mirror status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state, reason string
		var count int64
		if err := rows.Scan(&state, &reason, &count); err != nil {
			return result, fmt.Errorf("scan attachment mirror status: %w", err)
		}
		switch state {
		case "sending":
			result.Sending += count
		case "delivered":
			result.Delivered += count
		case "failed":
			result.Failed += count
		}
		if reason != "" {
			result.Reasons[reason] += count
		}
	}
	return result, rows.Err()
}

// RemoveInactiveDocbankAttachmentConsumers releases journal retention for retired destinations.
func (s *Store) RemoveInactiveDocbankAttachmentConsumers(ctx context.Context, destination string) error {
	active := DocbankAttachmentConsumerKey(destination)
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`
			DELETE FROM archive_metadata WHERE key IN (
				SELECT ? || SUBSTR(consumer_key, ?) FROM attachment_change_consumers
				WHERE consumer_key LIKE 'docbank-mirror:%' AND consumer_key <> ?)`,
			beeperMediaScanKey(""), len(DocbankAttachmentConsumerKey(""))+1, active); err != nil {
			return fmt.Errorf("remove inactive attachment mirror scans: %w", err)
		}
		result, err := q.Exec(`
			DELETE FROM attachment_change_consumers
			WHERE consumer_key LIKE 'docbank-mirror:%' AND consumer_key <> ?`, active)
		if err != nil {
			return fmt.Errorf("remove inactive attachment mirror consumers: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if s.IsPostgreSQL() {
			if _, err := q.Exec(`LOCK TABLE attachments, messages IN SHARE MODE`); err != nil {
				return fmt.Errorf("lock attachment mirror unregistration boundary: %w", err)
			}
		}
		return pruneAttachmentChanges(q)
	})
}

func DocbankAttachmentConsumerKey(destination string) string { return "docbank-mirror:" + destination }

// DocbankAttachmentCandidate returns an eligible attachment, or sql.ErrNoRows.
func (s *Store) DocbankAttachmentCandidate(ctx context.Context, id int64) (DocbankAttachmentDelivery, error) {
	var d DocbankAttachmentDelivery
	var storagePath string
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT a.id, COALESCE(a.content_hash,''), a.storage_path,
		       COALESCE(a.filename,''), COALESCE(a.mime_type,'')
		FROM attachments a
		WHERE a.id = ? AND `+docbankAttachmentEligibleWhere()), id).
		Scan(&d.AttachmentID, &d.ContentHash, &storagePath, &d.Name, &d.MIMEType)
	d.ContentHash = strings.ToLower(AttachmentContentHash(d.ContentHash, storagePath))
	return d, err
}
