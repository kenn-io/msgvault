package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ConversationPolicyPersonVersion is the exact semantic input sent to a provider.
type ConversationPolicyPersonVersion struct {
	PersonID int64
	Revision string
}

// CheckConversationPolicyPeopleTx validates original work and semantic authority
// through the caller's transaction. The caller already owns the policy fence.
func (s *Store) CheckConversationPolicyPeopleTx(ctx context.Context, tx ConversationPolicyTx, expectedRevision int64, persons []ConversationPolicyPersonVersion) error {
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&revision); err != nil {
		return err
	}
	if revision != expectedRevision {
		return ErrConversationPolicyStale
	}
	return s.CheckConversationPolicyPersonPublicationTx(ctx, tx, persons)
}

// CheckConversationPolicyPersonPublicationTx preserves the context's original
// policy revision and compares current canonical semantic documents. It never
// opens a second connection or captures a replacement work revision.
func (s *Store) CheckConversationPolicyPersonPublicationTx(ctx context.Context, tx ConversationPolicyTx, persons []ConversationPolicyPersonVersion) error {
	if err := s.CheckConversationPolicyPublicationTx(ctx, tx, nil); err != nil {
		return err
	}
	raw, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("person policy validation requires the caller SQL transaction")
	}
	for _, person := range persons {
		doc, err := s.LoadPersonSemanticDocumentTx(ctx, raw, person.PersonID)
		if errors.Is(err, ErrPersonNotFound) {
			return fmt.Errorf("%w: person %d", ErrConversationPolicyStale, person.PersonID)
		}
		if err != nil {
			return err
		}
		if doc.Text == "" || doc.Revision != person.Revision {
			return fmt.Errorf("%w: person %d semantic input", ErrConversationPolicyStale, person.PersonID)
		}
	}
	return nil
}

// CheckSQLiteConversationPolicyPeople closes its short archive transaction
// before native SQLite publication starts. The caller retains the file fence.
func (s *Store) CheckSQLiteConversationPolicyPeople(ctx context.Context, persons []ConversationPolicyPersonVersion) error {
	if s.IsPostgreSQL() {
		return errors.New("SQLite person check used with PostgreSQL")
	}
	tx, err := s.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return s.CheckConversationPolicyPersonPublicationTx(ctx, tx, persons)
}

type conversationPolicyPeopleKey struct{}

// WithConversationPolicyPeople binds exact input versions for each HTTP retry.
// The original policy context is retained; no live version is substituted.
func WithConversationPolicyPeople(ctx context.Context, versions []ConversationPolicyPersonVersion) context.Context {
	return context.WithValue(ctx, conversationPolicyPeopleKey{}, append([]ConversationPolicyPersonVersion(nil), versions...))
}

// LoadPersonSemanticDocumentTx renders current input on the caller's transaction.
// Cleanup uses this current-state authority without an earlier work revision.
func (s *Store) LoadPersonSemanticDocumentTx(ctx context.Context, tx *sql.Tx, personID int64) (*PersonSemanticDocument, error) {
	return s.loadPersonSemanticDocumentLoggedTx(ctx, &loggedTx{Tx: tx, rebind: s.Rebind}, personID)
}

func (s *Store) loadPersonSemanticDocumentLoggedTx(ctx context.Context, logged *loggedTx, personID int64) (*PersonSemanticDocument, error) {
	allowed, err := s.loadPersonSemanticAttributeDefinitionsTx(ctx, logged)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.loadPersonSemanticSnapshotTx(ctx, logged, personID, allowed)
	if err != nil {
		return nil, err
	}
	return renderPersonSemanticDocument(snapshot)
}
