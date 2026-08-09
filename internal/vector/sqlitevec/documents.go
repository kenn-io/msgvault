//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

var _ vector.DocumentPublisher = (*Backend)(nil)
var _ vector.DocumentJournalLifecycle = (*Backend)(nil)

// PublishScope replaces the complete current document set for scopeKey in one
// vectors.db transaction. It deletes vectors only through membership rows
// that still name an old document, then assigns replacement ownership before
// committing the new ledger state.
func (b *Backend) PublishScope(ctx context.Context, gen vector.GenerationID, scopeKey string, sourceSequence int64, docs []vector.DocumentPublication, chunks []vector.Chunk) error {
	return b.PublishScopes(ctx, gen, []vector.DocumentScopePublication{{
		ScopeKey: scopeKey, SourceSequence: sourceSequence, Documents: docs, Chunks: chunks,
	}})
}

type validatedSQLiteScopePublication struct {
	publication vector.DocumentScopePublication
	docByMember map[int64]string
	desiredKeys []string
}

// PublishScopes atomically publishes a bounded set of affected scopes and
// refreshes generation accounting once for the whole batch.
func (b *Backend) PublishScopes(ctx context.Context, gen vector.GenerationID, scopes []vector.DocumentScopePublication) error {
	if len(scopes) == 0 {
		return nil
	}
	validated := make([]validatedSQLiteScopePublication, len(scopes))
	seenScopes := make(map[string]struct{}, len(scopes))
	for i, publication := range scopes {
		if publication.ScopeKey == "" {
			return errors.New("publish scope: empty scope key")
		}
		if _, exists := seenScopes[publication.ScopeKey]; exists {
			return fmt.Errorf("publish scopes: duplicate scope key %q", publication.ScopeKey)
		}
		seenScopes[publication.ScopeKey] = struct{}{}
		docByMember, desiredKeys, err := validateDocumentPublication(
			publication.SourceSequence, publication.Documents, publication.Chunks, publication.FenceOnly)
		if err != nil {
			return err
		}
		validated[i] = validatedSQLiteScopePublication{
			publication: publication, docByMember: docByMember, desiredKeys: desiredKeys,
		}
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin document publication: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var dim int
	var state vector.GenerationState
	err = tx.QueryRowContext(ctx,
		`SELECT dimension, state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state == vector.GenerationRetired {
		return fmt.Errorf("%w: %d", vector.ErrGenerationRetired, gen)
	}
	for _, scope := range validated {
		for _, chunk := range scope.publication.Chunks {
			if len(chunk.Vector) != dim {
				return fmt.Errorf("%w: chunk %d for msg %d has %d dims, gen has %d",
					vector.ErrDimensionMismatch, chunk.ChunkIndex, chunk.MessageID, len(chunk.Vector), dim)
			}
		}
	}

	now := time.Now().Unix()
	applied := false
	for _, scope := range validated {
		accepted, err := claimSQLiteScopeSequence(ctx, tx, gen, scope.publication.ScopeKey,
			scope.publication.SourceSequence)
		if err != nil {
			return err
		}
		if !accepted {
			continue
		}
		applied = true
		if scope.publication.FenceOnly {
			if err := fenceSQLiteScope(ctx, tx, gen, scope.publication, now); err != nil {
				return err
			}
			continue
		}
		if err := b.publishSQLiteScope(ctx, tx, gen, dim, scope, now); err != nil {
			return err
		}
	}
	if applied {
		if _, err := tx.ExecContext(ctx, `
		UPDATE index_generations
		   SET message_count = (
			SELECT COUNT(DISTINCT message_id) FROM embeddings WHERE generation_id = ?
		   )
		 WHERE id = ?`, int64(gen), int64(gen)); err != nil {
			return fmt.Errorf("refresh generation message count: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit document publication: %w", err)
	}
	return nil
}

func claimSQLiteScopeSequence(
	ctx context.Context, tx *sql.Tx, gen vector.GenerationID, scopeKey string, sourceSequence int64,
) (bool, error) {
	var appliedSequence int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO embedding_document_scopes (generation_id, scope_key, source_sequence)
		VALUES (?, ?, ?)
		ON CONFLICT (generation_id, scope_key) DO UPDATE
		SET source_sequence = excluded.source_sequence
		WHERE excluded.source_sequence >= embedding_document_scopes.source_sequence
		RETURNING source_sequence`, int64(gen), scopeKey, sourceSequence).Scan(&appliedSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim scope %q source sequence %d: %w", scopeKey, sourceSequence, err)
	}
	return true, nil
}

func (b *Backend) publishSQLiteScope(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, dim int,
	scope validatedSQLiteScopePublication, now int64) error {
	publication := scope.publication
	ownedIDs, err := sqliteOwnedMembersForPublication(ctx, tx, gen, publication.ScopeKey, scope.desiredKeys)
	if err != nil {
		return err
	}
	current, err := sqliteScopeDocumentsTx(ctx, tx, gen, publication.ScopeKey)
	if err != nil {
		return err
	}
	preserved, err := validatedPreservedSQLiteMembers(current, publication.Documents)
	if err != nil {
		return err
	}
	for messageID := range scope.docByMember {
		ownedIDs[messageID] = struct{}{}
	}
	for messageID := range preserved {
		delete(ownedIDs, messageID)
	}
	ids := sortedDocumentIDs(ownedIDs)
	if err := deleteForMessageIDs(ctx, tx, VectorTableName(dim), gen, ids); err != nil {
		return fmt.Errorf("clear replaced document vectors: %w", err)
	}

	// Membership must be removed before tombstoning because it is the durable
	// proof of current ownership. A later replay of an obsolete scope cannot
	// find or delete a member that another document has already claimed.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM embedding_document_members
		 WHERE generation_id = ?
		   AND document_key IN (
			SELECT document_key FROM embedding_documents
			 WHERE generation_id = ? AND scope_key = ?
		   )`, int64(gen), int64(gen), publication.ScopeKey); err != nil {
		return fmt.Errorf("clear old scope membership: %w", err)
	}
	for _, key := range scope.desiredKeys {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM embedding_document_members WHERE generation_id = ? AND document_key = ?`,
			int64(gen), key); err != nil {
			return fmt.Errorf("clear desired document %q membership: %w", key, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE embedding_documents
		   SET state = ?, source_sequence = ?, updated_at = ?
		 WHERE generation_id = ? AND scope_key = ? AND state = ?`,
		string(vector.DocumentTombstoned), publication.SourceSequence, now,
		int64(gen), publication.ScopeKey, string(vector.DocumentCurrent)); err != nil {
		return fmt.Errorf("tombstone old scope documents: %w", err)
	}

	for _, doc := range publication.Documents {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO embedding_documents
				(generation_id, document_key, kind, scope_key, state,
				 published_revision, source_sequence, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(generation_id, document_key) DO UPDATE SET
				kind = excluded.kind,
				scope_key = excluded.scope_key,
				state = excluded.state,
				published_revision = excluded.published_revision,
				source_sequence = excluded.source_sequence,
				updated_at = excluded.updated_at`,
			int64(gen), doc.Key, doc.Kind, publication.ScopeKey, string(vector.DocumentCurrent),
			doc.Revision, doc.SourceSequence, now); err != nil {
			return fmt.Errorf("upsert document %q: %w", doc.Key, err)
		}
		for ordinal, messageID := range doc.Members {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM embedding_document_members WHERE generation_id = ? AND message_id = ?`,
				int64(gen), messageID); err != nil {
				return fmt.Errorf("release prior owner for message %d: %w", messageID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO embedding_document_members
					(generation_id, message_id, document_key, member_ordinal)
				VALUES (?, ?, ?, ?)`, int64(gen), messageID, doc.Key, ordinal); err != nil {
				return fmt.Errorf("assign message %d to document %q: %w", messageID, doc.Key, err)
			}
		}
	}

	return sqliteInsertDocumentChunks(ctx, tx, gen, dim, publication.Chunks, now)
}

func validateDocumentPublication(
	sourceSequence int64, docs []vector.DocumentPublication, chunks []vector.Chunk, fenceOnly bool,
) (map[int64]string, []string, error) {
	if fenceOnly && len(chunks) != 0 {
		return nil, nil, errors.New("publish scope: fence-only publication cannot contain chunks")
	}
	docByMember := make(map[int64]string)
	preserved := make(map[int64]struct{})
	seenKeys := make(map[string]struct{}, len(docs))
	keys := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.SourceSequence != sourceSequence {
			return nil, nil, fmt.Errorf("publish scope: document %q source sequence %d does not match scope sequence %d", doc.Key, doc.SourceSequence, sourceSequence)
		}
		if doc.Key == "" || doc.Kind == "" || doc.Revision == "" {
			return nil, nil, errors.New("publish scope: document key, kind, and revision are required")
		}
		if _, exists := seenKeys[doc.Key]; exists {
			return nil, nil, fmt.Errorf("publish scope: duplicate document key %q", doc.Key)
		}
		seenKeys[doc.Key] = struct{}{}
		keys = append(keys, doc.Key)
		for _, messageID := range doc.Members {
			if owner, exists := docByMember[messageID]; exists {
				return nil, nil, fmt.Errorf("publish scope: message %d belongs to both %q and %q", messageID, owner, doc.Key)
			}
			docByMember[messageID] = doc.Key
			if doc.PreserveVectors {
				preserved[messageID] = struct{}{}
			}
		}
	}
	chunked := make(map[int64]struct{}, len(chunks))
	for _, chunk := range chunks {
		if _, exists := docByMember[chunk.MessageID]; !exists {
			return nil, nil, fmt.Errorf("publish scope: chunk message %d has no desired document owner", chunk.MessageID)
		}
		if _, exists := preserved[chunk.MessageID]; exists {
			return nil, nil, fmt.Errorf("publish scope: preserved document member %d also has a replacement chunk", chunk.MessageID)
		}
		chunked[chunk.MessageID] = struct{}{}
	}
	for messageID := range preserved {
		chunked[messageID] = struct{}{}
	}
	if !fenceOnly {
		for messageID := range docByMember {
			if _, exists := chunked[messageID]; !exists {
				return nil, nil, fmt.Errorf("publish scope: document member %d has no chunk", messageID)
			}
		}
	}
	slices.Sort(keys)
	return docByMember, keys, nil
}

func validatedPreservedSQLiteMembers(
	current []vector.DocumentRecord, desired []vector.DocumentPublication,
) (map[int64]struct{}, error) {
	currentByKey := make(map[string]vector.DocumentRecord, len(current))
	for _, record := range current {
		currentByKey[record.Key] = record
	}
	preserved := make(map[int64]struct{})
	for _, doc := range desired {
		if !doc.PreserveVectors {
			continue
		}
		record, ok := currentByKey[doc.Key]
		if !ok || record.Kind != doc.Kind || record.PublishedRevision != doc.Revision ||
			!slices.Equal(record.Members, doc.Members) {
			return nil, fmt.Errorf("%w: preserved document %q changed", vector.ErrDocumentFenceChanged, doc.Key)
		}
		for _, messageID := range doc.Members {
			preserved[messageID] = struct{}{}
		}
	}
	return preserved, nil
}

func fenceSQLiteScope(
	ctx context.Context, tx *sql.Tx, gen vector.GenerationID,
	publication vector.DocumentScopePublication, now int64,
) error {
	current, err := sqliteScopeDocumentsTx(ctx, tx, gen, publication.ScopeKey)
	if err != nil {
		return err
	}
	if !sameFenceDocuments(current, publication.Documents) {
		return fmt.Errorf("%w: %q", vector.ErrDocumentFenceChanged, publication.ScopeKey)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE embedding_documents
		   SET source_sequence = ?, updated_at = ?
		 WHERE generation_id = ? AND scope_key = ? AND state = ?`,
		publication.SourceSequence, now, int64(gen), publication.ScopeKey,
		string(vector.DocumentCurrent)); err != nil {
		return fmt.Errorf("advance scope %q document fence: %w", publication.ScopeKey, err)
	}
	return nil
}

func sqliteScopeDocumentsTx(
	ctx context.Context, tx *sql.Tx, gen vector.GenerationID, scopeKey string,
) ([]vector.DocumentRecord, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.document_key, d.kind, d.published_revision, m.message_id
		  FROM embedding_documents d
		  LEFT JOIN embedding_document_members m
		    ON m.generation_id = d.generation_id AND m.document_key = d.document_key
		 WHERE d.generation_id = ? AND d.scope_key = ? AND d.state = ?
		 ORDER BY d.document_key, m.member_ordinal`, int64(gen), scopeKey, string(vector.DocumentCurrent))
	if err != nil {
		return nil, fmt.Errorf("read scope %q document fence: %w", scopeKey, err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]vector.DocumentRecord, 0)
	for rows.Next() {
		var key, kind, revision string
		var member sql.NullInt64
		if err := rows.Scan(&key, &kind, &revision, &member); err != nil {
			return nil, fmt.Errorf("scan scope %q document fence: %w", scopeKey, err)
		}
		if len(records) == 0 || records[len(records)-1].Key != key {
			records = append(records, vector.DocumentRecord{Key: key, Kind: kind,
				PublishedRevision: revision, Members: []int64{}})
		}
		if member.Valid {
			records[len(records)-1].Members = append(records[len(records)-1].Members, member.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read scope %q document fence rows: %w", scopeKey, err)
	}
	return records, nil
}

func sameFenceDocuments(current []vector.DocumentRecord, desired []vector.DocumentPublication) bool {
	if len(current) != len(desired) {
		return false
	}
	desiredByKey := make(map[string]vector.DocumentPublication, len(desired))
	for _, doc := range desired {
		desiredByKey[doc.Key] = doc
	}
	for _, record := range current {
		doc, ok := desiredByKey[record.Key]
		if !ok || record.Kind != doc.Kind || record.PublishedRevision != doc.Revision ||
			!slices.Equal(record.Members, doc.Members) {
			return false
		}
	}
	return true
}

func sqliteOwnedMembersForPublication(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, scopeKey string, desiredKeys []string) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.message_id
		  FROM embedding_document_members m
		  JOIN embedding_documents d
		    ON d.generation_id = m.generation_id AND d.document_key = m.document_key
		 WHERE m.generation_id = ?
		   AND (d.scope_key = ? OR m.document_key IN (`+sqlitePlaceholders(len(desiredKeys))+`))`,
		append([]any{int64(gen), scopeKey}, stringsToAny(desiredKeys)...)...)
	if err != nil {
		return nil, fmt.Errorf("list owned document members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owned := make(map[int64]struct{})
	for rows.Next() {
		var messageID int64
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("scan owned document member: %w", err)
		}
		owned[messageID] = struct{}{}
	}
	return owned, rows.Err()
}

func sqlitePlaceholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return "?" + strings.Repeat(",?", n-1)
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func sortedDocumentIDs(ids map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func sqliteInsertDocumentChunks(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, dim int, chunks []vector.Chunk, now int64) error {
	if len(chunks) == 0 {
		return nil
	}
	embedStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO embeddings
			(generation_id, message_id, chunk_index, embedded_at, source_char_len,
			 chunk_char_start, chunk_char_end, source_basis, truncated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING embedding_id`)
	if err != nil {
		return fmt.Errorf("prepare document embedding insert: %w", err)
	}
	defer func() { _ = embedStmt.Close() }()
	vecStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (generation_id, embedding_id, embedding) VALUES (?, ?, ?)`, VectorTableName(dim)))
	if err != nil {
		return fmt.Errorf("prepare document vector insert: %w", err)
	}
	defer func() { _ = vecStmt.Close() }()
	for _, chunk := range chunks {
		truncated := 0
		if chunk.Truncated {
			truncated = 1
		}
		var embeddingID int64
		if err := embedStmt.QueryRowContext(ctx,
			int64(gen), chunk.MessageID, chunk.ChunkIndex, now, chunk.SourceCharLen,
			chunk.ChunkCharStart, chunk.ChunkCharEnd, int(chunk.SourceBasis), truncated,
		).Scan(&embeddingID); err != nil {
			return fmt.Errorf("insert document embedding (msg %d chunk %d): %w", chunk.MessageID, chunk.ChunkIndex, err)
		}
		if _, err := vecStmt.ExecContext(ctx, int64(gen), embeddingID, float32SliceBlob(chunk.Vector)); err != nil {
			return fmt.Errorf("insert document vector (msg %d chunk %d): %w", chunk.MessageID, chunk.ChunkIndex, err)
		}
	}
	return nil
}

func (b *Backend) GetDocument(ctx context.Context, gen vector.GenerationID, key string) (vector.DocumentRecord, error) {
	row := b.db.QueryRowContext(ctx, `
		SELECT document_key, kind, scope_key, state, published_revision, source_sequence
		  FROM embedding_documents
		 WHERE generation_id = ? AND document_key = ?`, int64(gen), key)
	record, err := scanSQLiteDocument(row, gen)
	if err != nil {
		return vector.DocumentRecord{}, err
	}
	record.Members, err = b.sqliteDocumentMembers(ctx, gen, record.Key)
	return record, err
}

func (b *Backend) ListDocumentsForScope(ctx context.Context, gen vector.GenerationID, scopeKey string) ([]vector.DocumentRecord, error) {
	return b.listSQLiteDocuments(ctx, gen, `scope_key = ? AND state = ?`, []any{scopeKey, string(vector.DocumentCurrent)}, 0)
}

func (b *Backend) ListDocumentsAfter(ctx context.Context, gen vector.GenerationID, afterKey string, limit int) ([]vector.DocumentRecord, error) {
	if limit <= 0 {
		return []vector.DocumentRecord{}, nil
	}
	return b.listSQLiteDocuments(ctx, gen, `document_key > ? AND state = ?`, []any{afterKey, string(vector.DocumentCurrent)}, limit)
}

type sqliteDocumentScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteDocument(row sqliteDocumentScanner, gen vector.GenerationID) (vector.DocumentRecord, error) {
	record := vector.DocumentRecord{GenerationID: gen, Members: []int64{}}
	if err := row.Scan(&record.Key, &record.Kind, &record.ScopeKey, &record.State,
		&record.PublishedRevision, &record.SourceSequence); err != nil {
		return vector.DocumentRecord{}, err
	}
	return record, nil
}

func (b *Backend) listSQLiteDocuments(ctx context.Context, gen vector.GenerationID, predicate string, args []any, limit int) ([]vector.DocumentRecord, error) {
	query := `SELECT document_key, kind, scope_key, state, published_revision, source_sequence
		FROM embedding_documents WHERE generation_id = ? AND ` + predicate + ` ORDER BY document_key`
	allArgs := append([]any{int64(gen)}, args...)
	if limit > 0 {
		query += ` LIMIT ?`
		allArgs = append(allArgs, limit)
	}
	rows, err := b.db.QueryContext(ctx, query, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]vector.DocumentRecord, 0)
	for rows.Next() {
		record, err := scanSQLiteDocument(rows, gen)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan document: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range records {
		records[i].Members, err = b.sqliteDocumentMembers(ctx, gen, records[i].Key)
		if err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (b *Backend) sqliteDocumentMembers(ctx context.Context, gen vector.GenerationID, key string) ([]int64, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT message_id FROM embedding_document_members
		 WHERE generation_id = ? AND document_key = ? ORDER BY member_ordinal`, int64(gen), key)
	if err != nil {
		return nil, fmt.Errorf("list members for document %q: %w", key, err)
	}
	defer func() { _ = rows.Close() }()
	members := make([]int64, 0)
	for rows.Next() {
		var messageID int64
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("scan member for document %q: %w", key, err)
		}
		members = append(members, messageID)
	}
	return members, rows.Err()
}

func (b *Backend) GetDocumentProgress(ctx context.Context, gen vector.GenerationID) (vector.DocumentProgress, error) {
	var progress vector.DocumentProgress
	err := b.db.QueryRowContext(ctx, `
		SELECT change_sequence, reconcile_cursor, journal_cursor
		  FROM embedding_document_progress WHERE generation_id = ?`, int64(gen)).
		Scan(&progress.ChangeSequence, &progress.ReconcileCursor, &progress.JournalCursor)
	if errors.Is(err, sql.ErrNoRows) {
		return vector.DocumentProgress{}, nil
	}
	if err != nil {
		return vector.DocumentProgress{}, fmt.Errorf("get document progress for generation %d: %w", gen, err)
	}
	return progress, nil
}

func (b *Backend) AdvanceDocumentChangeWatermark(ctx context.Context, gen vector.GenerationID, sequence int64) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO embedding_document_progress (generation_id, change_sequence)
		VALUES (?, ?)
		ON CONFLICT(generation_id) DO UPDATE SET
			change_sequence = MAX(change_sequence, excluded.change_sequence)`, int64(gen), sequence)
	if err != nil {
		return fmt.Errorf("advance document change watermark: %w", err)
	}
	return nil
}

func (b *Backend) SetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID, cursor string) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO embedding_document_progress (generation_id, reconcile_cursor)
		VALUES (?, ?)
		ON CONFLICT(generation_id) DO UPDATE SET reconcile_cursor = excluded.reconcile_cursor`,
		int64(gen), cursor)
	if err != nil {
		return fmt.Errorf("set document reconcile cursor: %w", err)
	}
	return nil
}

func (b *Backend) SetDocumentJournalCursor(ctx context.Context, gen vector.GenerationID, cursor string) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO embedding_document_progress (generation_id, journal_cursor)
		VALUES (?, ?)
		ON CONFLICT(generation_id) DO UPDATE SET journal_cursor = excluded.journal_cursor`,
		int64(gen), cursor)
	if err != nil {
		return fmt.Errorf("set document journal cursor: %w", err)
	}
	return nil
}

func (b *Backend) ResetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID) error {
	return b.SetDocumentReconcileCursor(ctx, gen, "")
}

func (b *Backend) MinimumDocumentChangeWatermark(ctx context.Context) (int64, bool, error) {
	var sequence sql.NullInt64
	err := b.db.QueryRowContext(ctx, `
		SELECT MIN(p.change_sequence)
		  FROM embedding_document_progress p
		  JOIN index_generations g ON g.id = p.generation_id
		 WHERE g.state IN ('active', 'building')`).Scan(&sequence)
	if err != nil {
		return 0, false, fmt.Errorf("read minimum contextual journal watermark: %w", err)
	}
	return sequence.Int64, sequence.Valid, nil
}

func (b *Backend) CleanupDocumentJournalIfUnused(ctx context.Context) error {
	if b.mainPath == "" || b.readOnly {
		return nil
	}
	conn, err := b.openFusedConn(ctx)
	if err != nil {
		return fmt.Errorf("open coordinated contextual journal cleanup: %w", err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin contextual journal cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var schemaReady int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'embedding_change_clock')
		   AND EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'embedding_changes')`).Scan(&schemaReady); err != nil {
		return fmt.Errorf("inspect contextual journal schema: %w", err)
	}
	if schemaReady == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE embedding_change_clock SET sequence = sequence WHERE singleton = 1`); err != nil {
		return fmt.Errorf("lock contextual journal for cleanup: %w", err)
	}
	var tracked int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM vec.embedding_document_progress p
			  JOIN vec.index_generations g ON g.id = p.generation_id
			 WHERE g.state IN ('active', 'building')
		)`).Scan(&tracked); err != nil {
		return fmt.Errorf("recheck live contextual generations: %w", err)
	}
	if tracked != 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE embedding_change_clock SET enabled = FALSE WHERE singleton = 1`); err != nil {
		return fmt.Errorf("disable unused contextual journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_changes`); err != nil {
		return fmt.Errorf("prune unused contextual journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit contextual journal cleanup: %w", err)
	}
	return nil
}
