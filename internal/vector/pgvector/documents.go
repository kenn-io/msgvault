//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

var _ vector.DocumentPublisher = (*Backend)(nil)
var _ vector.DocumentJournalLifecycle = (*Backend)(nil)

func (b *Backend) PublishScope(ctx context.Context, gen vector.GenerationID, scopeKey string, sourceSequence int64, docs []vector.DocumentPublication, chunks []vector.Chunk) error {
	return b.PublishScopes(ctx, gen, []vector.DocumentScopePublication{{
		ScopeKey: scopeKey, SourceSequence: sourceSequence, Documents: docs, Chunks: chunks,
	}})
}

type validatedPGScopePublication struct {
	publication vector.DocumentScopePublication
	docByMember map[int64]string
	desiredKeys []string
}

func (b *Backend) PublishScopes(ctx context.Context, gen vector.GenerationID, scopes []vector.DocumentScopePublication) error {
	if len(scopes) == 0 {
		return nil
	}
	validated := make([]validatedPGScopePublication, len(scopes))
	seenScopes := make(map[string]struct{}, len(scopes))
	for i, publication := range scopes {
		if publication.ScopeKey == "" {
			return errors.New("publish scope: empty scope key")
		}
		if _, exists := seenScopes[publication.ScopeKey]; exists {
			return fmt.Errorf("publish scopes: duplicate scope key %q", publication.ScopeKey)
		}
		seenScopes[publication.ScopeKey] = struct{}{}
		docByMember, desiredKeys, err := validatePGDocumentPublication(
			publication.SourceSequence, publication.Documents, publication.Chunks, publication.FenceOnly)
		if err != nil {
			return err
		}
		validated[i] = validatedPGScopePublication{
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
		`SELECT dimension, state FROM index_generations WHERE id = $1 FOR UPDATE`, int64(gen)).Scan(&dim, &state)
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
		accepted, err := claimPGScopeSequence(ctx, tx, gen, scope.publication.ScopeKey,
			scope.publication.SourceSequence)
		if err != nil {
			return err
		}
		if !accepted {
			continue
		}
		applied = true
		if scope.publication.FenceOnly {
			if err := fencePGScope(ctx, tx, gen, scope.publication, now); err != nil {
				return err
			}
			continue
		}
		if err := b.publishPGScope(ctx, tx, gen, dim, scope, now); err != nil {
			return err
		}
	}
	if applied {
		if _, err := tx.ExecContext(ctx, `
		UPDATE index_generations
		   SET message_count = (
			SELECT COUNT(DISTINCT message_id) FROM embeddings WHERE generation_id = $1
		   )
		 WHERE id = $1`, int64(gen)); err != nil {
			return fmt.Errorf("refresh generation message count: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit document publication: %w", err)
	}
	return nil
}

func claimPGScopeSequence(
	ctx context.Context, tx *sql.Tx, gen vector.GenerationID, scopeKey string, sourceSequence int64,
) (bool, error) {
	var appliedSequence int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO embedding_document_scopes (generation_id, scope_key, source_sequence)
		VALUES ($1, $2, $3)
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

func (b *Backend) publishPGScope(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, dim int,
	scope validatedPGScopePublication, now int64) error {
	publication := scope.publication
	owned, err := pgOwnedMembersForPublication(ctx, tx, gen, publication.ScopeKey, scope.desiredKeys)
	if err != nil {
		return err
	}
	current, err := pgScopeDocumentsTx(ctx, tx, gen, publication.ScopeKey)
	if err != nil {
		return err
	}
	preserved, err := validatedPreservedPGMembers(current, publication.Documents)
	if err != nil {
		return err
	}
	for messageID := range scope.docByMember {
		owned[messageID] = struct{}{}
	}
	for messageID := range preserved {
		delete(owned, messageID)
	}
	ids := sortedPGDocumentIDs(owned)
	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM embeddings WHERE generation_id = $1 AND message_id = ANY($2::bigint[])`,
			int64(gen), int64Array(ids)); err != nil {
			return fmt.Errorf("clear replaced document vectors: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM embedding_document_members m
		USING embedding_documents d
		WHERE m.generation_id = $1
		  AND d.generation_id = m.generation_id
		  AND d.document_key = m.document_key
		  AND d.scope_key = $2`, int64(gen), publication.ScopeKey); err != nil {
		return fmt.Errorf("clear old scope membership: %w", err)
	}
	if len(scope.desiredKeys) > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM embedding_document_members
			WHERE generation_id = $1 AND document_key = ANY($2::text[])`,
			int64(gen), textArray(scope.desiredKeys)); err != nil {
			return fmt.Errorf("clear desired document membership: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE embedding_documents
		   SET state = $1, source_sequence = $2, updated_at = $3
		 WHERE generation_id = $4 AND scope_key = $5 AND state = $6`,
		string(vector.DocumentTombstoned), publication.SourceSequence, now,
		int64(gen), publication.ScopeKey, string(vector.DocumentCurrent)); err != nil {
		return fmt.Errorf("tombstone old scope documents: %w", err)
	}

	for _, doc := range publication.Documents {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO embedding_documents
				(generation_id, document_key, kind, scope_key, state,
				 published_revision, source_sequence, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
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
				`DELETE FROM embedding_document_members WHERE generation_id = $1 AND message_id = $2`,
				int64(gen), messageID); err != nil {
				return fmt.Errorf("release prior owner for message %d: %w", messageID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO embedding_document_members
					(generation_id, message_id, document_key, member_ordinal)
				VALUES ($1, $2, $3, $4)`, int64(gen), messageID, doc.Key, ordinal); err != nil {
				return fmt.Errorf("assign message %d to document %q: %w", messageID, doc.Key, err)
			}
		}
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO embeddings
			(generation_id, message_id, chunk_index, embedded_at, source_char_len,
			 chunk_char_start, chunk_char_end, source_basis, truncated, dimension, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::vector)`)
	if err != nil {
		return fmt.Errorf("prepare document embedding insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, chunk := range publication.Chunks {
		if _, err := stmt.ExecContext(ctx,
			int64(gen), chunk.MessageID, chunk.ChunkIndex, now, chunk.SourceCharLen,
			chunk.ChunkCharStart, chunk.ChunkCharEnd, int(chunk.SourceBasis), chunk.Truncated,
			dim, vectorLiteral(chunk.Vector)); err != nil {
			return fmt.Errorf("insert document embedding (msg %d chunk %d): %w", chunk.MessageID, chunk.ChunkIndex, err)
		}
	}
	return nil
}

func validatePGDocumentPublication(
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

func validatedPreservedPGMembers(
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

func fencePGScope(
	ctx context.Context, tx *sql.Tx, gen vector.GenerationID,
	publication vector.DocumentScopePublication, now int64,
) error {
	current, err := pgScopeDocumentsTx(ctx, tx, gen, publication.ScopeKey)
	if err != nil {
		return err
	}
	if !samePGFenceDocuments(current, publication.Documents) {
		return fmt.Errorf("%w: %q", vector.ErrDocumentFenceChanged, publication.ScopeKey)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE embedding_documents
		   SET source_sequence = $1, updated_at = $2
		 WHERE generation_id = $3 AND scope_key = $4 AND state = $5`,
		publication.SourceSequence, now, int64(gen), publication.ScopeKey,
		string(vector.DocumentCurrent)); err != nil {
		return fmt.Errorf("advance scope %q document fence: %w", publication.ScopeKey, err)
	}
	return nil
}

func pgScopeDocumentsTx(
	ctx context.Context, tx *sql.Tx, gen vector.GenerationID, scopeKey string,
) ([]vector.DocumentRecord, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.document_key, d.kind, d.published_revision, m.message_id
		  FROM embedding_documents d
		  LEFT JOIN embedding_document_members m
		    ON m.generation_id = d.generation_id AND m.document_key = d.document_key
		 WHERE d.generation_id = $1 AND d.scope_key = $2 AND d.state = $3
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

func samePGFenceDocuments(current []vector.DocumentRecord, desired []vector.DocumentPublication) bool {
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

func pgOwnedMembersForPublication(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, scopeKey string, desiredKeys []string) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.message_id
		  FROM embedding_document_members m
		  JOIN embedding_documents d
		    ON d.generation_id = m.generation_id AND d.document_key = m.document_key
		 WHERE m.generation_id = $1
		   AND (d.scope_key = $2 OR m.document_key = ANY($3::text[]))`,
		int64(gen), scopeKey, textArray(desiredKeys))
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

func sortedPGDocumentIDs(ids map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (b *Backend) GetDocument(ctx context.Context, gen vector.GenerationID, key string) (vector.DocumentRecord, error) {
	row := b.db.QueryRowContext(ctx, `
		SELECT document_key, kind, scope_key, state, published_revision, source_sequence
		  FROM embedding_documents
		 WHERE generation_id = $1 AND document_key = $2`, int64(gen), key)
	record, err := scanPGDocument(row, gen)
	if err != nil {
		return vector.DocumentRecord{}, err
	}
	record.Members, err = b.pgDocumentMembers(ctx, gen, record.Key)
	return record, err
}

func (b *Backend) ListDocumentsForScope(ctx context.Context, gen vector.GenerationID, scopeKey string) ([]vector.DocumentRecord, error) {
	return b.listPGDocuments(ctx, gen, `scope_key = $2 AND state = $3`, []any{scopeKey, string(vector.DocumentCurrent)}, 0)
}

func (b *Backend) ListDocumentsAfter(ctx context.Context, gen vector.GenerationID, afterKey string, limit int) ([]vector.DocumentRecord, error) {
	if limit <= 0 {
		return []vector.DocumentRecord{}, nil
	}
	return b.listPGDocuments(ctx, gen, `document_key > $2 AND state = $3`, []any{afterKey, string(vector.DocumentCurrent)}, limit)
}

type pgDocumentScanner interface {
	Scan(dest ...any) error
}

func scanPGDocument(row pgDocumentScanner, gen vector.GenerationID) (vector.DocumentRecord, error) {
	record := vector.DocumentRecord{GenerationID: gen, Members: []int64{}}
	if err := row.Scan(&record.Key, &record.Kind, &record.ScopeKey, &record.State,
		&record.PublishedRevision, &record.SourceSequence); err != nil {
		return vector.DocumentRecord{}, err
	}
	return record, nil
}

func (b *Backend) listPGDocuments(ctx context.Context, gen vector.GenerationID, predicate string, args []any, limit int) ([]vector.DocumentRecord, error) {
	query := `SELECT document_key, kind, scope_key, state, published_revision, source_sequence
		FROM embedding_documents WHERE generation_id = $1 AND ` + predicate + ` ORDER BY document_key`
	allArgs := append([]any{int64(gen)}, args...)
	if limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d`, len(allArgs)+1)
		allArgs = append(allArgs, limit)
	}
	rows, err := b.db.QueryContext(ctx, query, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]vector.DocumentRecord, 0)
	for rows.Next() {
		record, err := scanPGDocument(rows, gen)
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
		records[i].Members, err = b.pgDocumentMembers(ctx, gen, records[i].Key)
		if err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (b *Backend) pgDocumentMembers(ctx context.Context, gen vector.GenerationID, key string) ([]int64, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT message_id FROM embedding_document_members
		 WHERE generation_id = $1 AND document_key = $2 ORDER BY member_ordinal`, int64(gen), key)
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
		  FROM embedding_document_progress WHERE generation_id = $1`, int64(gen)).
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
		VALUES ($1, $2)
		ON CONFLICT(generation_id) DO UPDATE SET
			change_sequence = GREATEST(embedding_document_progress.change_sequence, excluded.change_sequence)`,
		int64(gen), sequence)
	if err != nil {
		return fmt.Errorf("advance document change watermark: %w", err)
	}
	return nil
}

func (b *Backend) SetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID, cursor string) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO embedding_document_progress (generation_id, reconcile_cursor)
		VALUES ($1, $2)
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
		VALUES ($1, $2)
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
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin contextual journal cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return fmt.Errorf("disable statement timeout for contextual journal cleanup: %w", err)
	}
	var schemaReady bool
	if err := tx.QueryRowContext(ctx, `
		SELECT to_regclass('embedding_change_clock') IS NOT NULL
		   AND to_regclass('embedding_changes') IS NOT NULL`).Scan(&schemaReady); err != nil {
		return fmt.Errorf("inspect contextual journal schema: %w", err)
	}
	if !schemaReady {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('msgvault.embedding_change_clock', 0))`); err != nil {
		return fmt.Errorf("lock contextual source mutations for journal cleanup: %w", err)
	}
	var journalSequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1 FOR UPDATE`).Scan(&journalSequence); err != nil {
		return fmt.Errorf("lock contextual journal for cleanup: %w", err)
	}
	var tracked bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM embedding_document_progress p
			  JOIN index_generations g ON g.id = p.generation_id
			 WHERE g.state IN ('active', 'building')
		)`).Scan(&tracked); err != nil {
		return fmt.Errorf("recheck live contextual generations: %w", err)
	}
	if tracked {
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
