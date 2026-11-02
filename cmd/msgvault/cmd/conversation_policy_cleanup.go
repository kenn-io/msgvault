package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// Handles already own their native/main binding. Pruners take W per bounded
// batch themselves; the coordinator must not hold an outer policy fence.
type conversationPolicyCleanupLanes struct {
	Embeddings    vector.IneligibleEmbeddingPruner
	Persons       vector.IneligiblePersonPruner
	Documents     vector.IneligibleDocumentPruner
	Visuals       vector.IneligibleVisualPruner
	nativeMissing bool
	sqlitePath    string
}

func reconcileConversationPolicyCleanup(ctx context.Context, st *store.Store, lanes conversationPolicyCleanupLanes) error {
	target, pending, err := st.ConversationPolicyCleanupPending(ctx)
	if err != nil || !pending {
		return err
	}
	if err := st.ReconcileConversationPolicyMediaProgress(ctx); err != nil {
		return fmt.Errorf("repair policy media progress: %w", err)
	}
	if lanes.nativeMissing {
		if err := st.ReconcileConversationPolicyMissingVectors(ctx, lanes.sqlitePath); err != nil {
			return err
		}
	} else {
		if lanes.Embeddings == nil || lanes.Persons == nil || lanes.Documents == nil || lanes.Visuals == nil {
			return errors.New("conversation-policy cleanup lane is unavailable")
		}
		if _, err := lanes.Embeddings.PruneIneligibleEmbeddings(ctx); err != nil {
			return fmt.Errorf("clean policy message vectors: %w", err)
		}
		if _, err := lanes.Persons.PruneIneligiblePersons(ctx); err != nil {
			return fmt.Errorf("clean policy person vectors: %w", err)
		}
		if _, err := lanes.Documents.PruneIneligibleDocuments(ctx); err != nil {
			return fmt.Errorf("clean policy document vectors: %w", err)
		}
		if _, err := lanes.Visuals.PruneIneligibleVisuals(ctx); err != nil {
			return fmt.Errorf("clean policy visual vectors: %w", err)
		}
	}
	_, err = st.AcknowledgeConversationPolicyCleanup(ctx, target)
	return err
}

// openConversationPolicyCleanupLanes opens only existing native storage. It
// does not consult feature enablement, credentials, consent, or create generations.
func openConversationPolicyCleanupLanes(ctx context.Context, st *store.Store) (conversationPolicyCleanupLanes, func() error, error) {
	closeNothing := func() error { return nil }
	if st == nil {
		return conversationPolicyCleanupLanes{}, nil, errors.New("policy cleanup requires an archive")
	}
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return conversationPolicyCleanupLanes{}, nil, errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	if st.IsPostgreSQL() {
		present, err := st.ConversationPolicyNativeVectorsPresent(ctx)
		if err != nil {
			return conversationPolicyCleanupLanes{}, nil, err
		}
		if !present {
			return conversationPolicyCleanupLanes{nativeMissing: true}, closeNothing, nil
		}
		// These existing flags skip initialization/backfill; the borrowed Store.DB
		// remains writable and each deletion uses its actual transaction.
		backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), PolicyStore: st, SkipMigrate: true, ReadOnly: true})
		if err != nil {
			return conversationPolicyCleanupLanes{}, nil, err
		}
		return conversationPolicyCleanupLanes{Embeddings: backend, Persons: backend, Documents: backend.DocumentBackend(), Visuals: backend.Visual()}, backend.Close, nil
	}
	dsn := cfg.Vector.DBPath
	if dsn == "" {
		dsn = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	resolvedDSN, filesystemPath, err := sqliteutil.ResolveDSN(dsn)
	if err != nil {
		return conversationPolicyCleanupLanes{}, nil, fmt.Errorf("resolve policy vector store path: %w", err)
	}
	if _, err := os.Stat(filesystemPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return conversationPolicyCleanupLanes{nativeMissing: true, sqlitePath: filesystemPath}, closeNothing, nil
		}
		return conversationPolicyCleanupLanes{}, nil, fmt.Errorf("stat policy vector store: %w", err)
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: resolvedDSN, ExistingOnly: true, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), PolicyStore: st, ReadOnly: true})
	if err != nil {
		return conversationPolicyCleanupLanes{}, nil, fmt.Errorf("open policy vector cleanup: %w", err)
	}
	return conversationPolicyCleanupLanes{Embeddings: backend, Persons: backend, Documents: backend.DocumentBackend(), Visuals: backend.Visual()}, backend.Close, nil
}
