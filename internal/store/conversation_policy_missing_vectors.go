package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// ConversationPolicyNativeVectorsPresent distinguishes a never-created PG
// native schema from a partial/unavailable lane. SQLite factories stat their file.
func (s *Store) ConversationPolicyNativeVectorsPresent(ctx context.Context) (bool, error) {
	if !s.IsPostgreSQL() {
		return false, errors.New("native schema presence requires PostgreSQL")
	}
	return conversationPolicyNativeVectorsPresent(ctx, s.DB())
}

func conversationPolicyNativeVectorsPresent(ctx context.Context, q ConversationPolicyTx) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (VALUES (to_regclass('index_generations')),(to_regclass('embeddings')),(to_regclass('person_embeddings')),(to_regclass('document_vector_embeddings')),(to_regclass('visual_vectors'))) AS native(name) WHERE name IS NOT NULL`).Scan(&count)
	if err != nil {
		return false, err
	}
	if count != 0 && count != 5 {
		return false, errors.New("native vector schema is incomplete")
	}
	return count == 5, nil
}

func (s *Store) confirmConversationPolicyVectorsMissing(ctx context.Context, tx *sql.Tx, path string) error {
	if s.IsPostgreSQL() {
		present, err := conversationPolicyNativeVectorsPresent(ctx, tx)
		if err != nil {
			return err
		}
		if present {
			return errors.New("native vectors appeared during cleanup; retry")
		}
		return nil
	}
	if path == "" {
		return errors.New("missing native cleanup requires the vector path")
	}
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check missing native vectors: %w", err)
	}
	return errors.New("native vectors appeared during cleanup; retry")
}

// ReconcileConversationPolicyMissingVectors revokes main-archive publication
// claims that cannot have a native vector. Absence is rechecked under W in each
// bounded batch; a newly created backend stops the pass without acknowledging it.
func (s *Store) ReconcileConversationPolicyMissingVectors(ctx context.Context, sqlitePath string) error {
	after := ""
	for {
		var page DocumentPolicyCleanupPage
		err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
			if err := s.confirmConversationPolicyVectorsMissing(ctx, tx.Tx, sqlitePath); err != nil {
				return err
			}
			var err error
			page, err = s.revokeDocumentPolicyTokens(ctx, tx.Tx, after, true)
			if err != nil {
				return err
			}
			return s.FinalizeDocumentPolicyTokens(ctx, tx.Tx, page.Tokens)
		})
		if err != nil {
			return err
		}
		if !page.More {
			break
		}
		after = page.AfterToken
	}
	visualAfter := VisualPolicyCleanupCursor{}
	for {
		var page VisualPolicyCleanupPage
		err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
			if err := s.confirmConversationPolicyVectorsMissing(ctx, tx.Tx, sqlitePath); err != nil {
				return err
			}
			var err error
			page, err = s.revokeVisualPolicyTokens(ctx, tx.Tx, visualAfter, true)
			if err != nil {
				return err
			}
			return s.FinalizeVisualPolicyTokens(ctx, tx.Tx, page.Tokens)
		})
		if err != nil {
			return err
		}
		if !page.More {
			break
		}
		visualAfter = page.After
	}
	obsoleteAfter := VisualPolicyToken{}
	for {
		var tokens []VisualPolicyToken
		err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
			if err := s.confirmConversationPolicyVectorsMissing(ctx, tx.Tx, sqlitePath); err != nil {
				return err
			}
			var err error
			tokens, err = s.ListVisualPolicyObsoleteTokens(ctx, tx.Tx, obsoleteAfter)
			if err != nil {
				return err
			}
			return s.FinalizeVisualPolicyTokens(ctx, tx.Tx, tokens)
		})
		if err != nil {
			return err
		}
		if len(tokens) < 500 {
			break
		}
		obsoleteAfter = tokens[len(tokens)-1]
	}
	var afterMessage int64
	for {
		scanned := 0
		err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
			if err := s.confirmConversationPolicyVectorsMissing(ctx, tx.Tx, sqlitePath); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE embed_gen IS NOT NULL AND id>? ORDER BY id LIMIT 500`, afterMessage)
			if err != nil {
				return err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			scanned = len(ids)
			for _, id := range ids {
				afterMessage = id
				if _, err := tx.ExecContext(ctx, `UPDATE messages SET embed_gen=NULL WHERE id=?`, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if scanned < 500 {
			return nil
		}
	}
}
