package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrGCUnsupported is returned before mutation when archive GC is requested
// against PostgreSQL. PostgreSQL retention and compaction require an
// operator-managed backup and VACUUM policy outside this SQLite command.
var ErrGCUnsupported = errors.New("msgvault gc is SQLite-only")

// GCPlan records both the destructive target and the adjacent population that
// must be retained. SourceDeleted is the only delete authority; deleted_at is
// the reversible dedup/hide marker and never makes a row purgeable here.
type GCPlan struct {
	SourceDeleted       int64   `json:"source_deleted"`
	DedupHiddenRetained int64   `json:"dedup_hidden_retained"`
	SourceDeletedIDs    []int64 `json:"-"`
}

// PlanGCContext counts the rows GC would purge and the dedup-only rows it will
// explicitly leave in the archive.
func (s *Store) PlanGCContext(ctx context.Context) (GCPlan, error) {
	if s.IsPostgreSQL() {
		return GCPlan{}, ErrGCUnsupported
	}
	return planGCWith(boundQuerier{ctx: ctx, q: s.db})
}

func planGCWith(q querier) (GCPlan, error) {
	var plan GCPlan
	if err := q.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE deleted_from_source_at IS NOT NULL),
			COUNT(*) FILTER (
				WHERE deleted_at IS NOT NULL
				  AND deleted_from_source_at IS NULL
			)
		FROM messages
	`).Scan(&plan.SourceDeleted, &plan.DedupHiddenRetained); err != nil {
		return GCPlan{}, fmt.Errorf("plan archive GC: %w", err)
	}
	ids, err := commaSeparatedIDs(q, `
		SELECT COALESCE(GROUP_CONCAT(id, ','), '')
		FROM (
			SELECT id FROM messages
			WHERE deleted_from_source_at IS NOT NULL
			ORDER BY id
		)
	`)
	if err != nil {
		return GCPlan{}, fmt.Errorf("plan archive GC message IDs: %w", err)
	}
	plan.SourceDeletedIDs = ids
	return plan, nil
}

func commaSeparatedIDs(q querier, query string) ([]int64, error) {
	var encoded string
	if err := q.QueryRow(query).Scan(&encoded); err != nil {
		return nil, err
	}
	if encoded == "" {
		return []int64{}, nil
	}
	parts := strings.Split(encoded, ",")
	ids := make([]int64, len(parts))
	for i, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse ID %q: %w", part, err)
		}
		ids[i] = id
	}
	return ids, nil
}

func equalGCPlans(a, b GCPlan) bool {
	return a.SourceDeleted == b.SourceDeleted &&
		a.DedupHiddenRetained == b.DedupHiddenRetained &&
		slices.Equal(a.SourceDeletedIDs, b.SourceDeletedIDs)
}

// ExecuteGCContext permanently removes source-deleted rows only. The expected
// plan is rechecked inside the deletion transaction so a confirmation cannot
// authorize a population that changed while the operator was reading it.
func (s *Store) ExecuteGCContext(
	ctx context.Context,
	expected GCPlan,
) (int64, error) {
	if s.IsPostgreSQL() {
		return 0, ErrGCUnsupported
	}

	var deleted int64
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		actual, err := planGCWith(q)
		if err != nil {
			return err
		}
		if !equalGCPlans(actual, expected) {
			return fmt.Errorf(
				"GC plan changed after confirmation: expected %+v, found %+v",
				expected,
				actual,
			)
		}
		affectedConversationIDs, err := commaSeparatedIDs(q, `
			SELECT COALESCE(GROUP_CONCAT(id, ','), '')
			FROM (
				SELECT DISTINCT conversation_id AS id
				FROM messages
				WHERE deleted_from_source_at IS NOT NULL
				  AND conversation_id IS NOT NULL
				ORDER BY conversation_id
			)
		`)
		if err != nil {
			return fmt.Errorf("list conversations affected by archive GC: %w", err)
		}

		if s.fts5Available {
			if _, err := q.Exec(`
				DELETE FROM messages_fts
				WHERE rowid IN (
					SELECT id FROM messages
					WHERE deleted_from_source_at IS NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("delete source-deleted FTS rows: %w", err)
			}
		}

		if _, err := q.Exec(`
			UPDATE messages
			SET reply_to_message_id = NULL
			WHERE reply_to_message_id IN (
				SELECT id FROM messages
				WHERE deleted_from_source_at IS NOT NULL
			)
		`); err != nil {
			return fmt.Errorf("clear replies to source-deleted messages: %w", err)
		}

		result, err := q.Exec(`
			DELETE FROM messages
			WHERE deleted_from_source_at IS NOT NULL
		`)
		if err != nil {
			return fmt.Errorf("delete source-deleted messages: %w", err)
		}
		deleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count source-deleted messages: %w", err)
		}
		if deleted != expected.SourceDeleted {
			return fmt.Errorf(
				"GC delete count changed: expected %d, deleted %d",
				expected.SourceDeleted,
				deleted,
			)
		}
		for _, conversationID := range affectedConversationIDs {
			if err := s.recomputeConversationStatsWith(
				q, "id = ?", conversationID,
			); err != nil {
				return fmt.Errorf(
					"refresh conversation %d after archive GC: %w",
					conversationID, err,
				)
			}
		}
		if deleted > 0 {
			if err := s.bumpDerivedDataRevision(tx); err != nil {
				return fmt.Errorf("invalidate caches after archive GC: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// VacuumContext compacts a SQLite archive after committed GC. VACUUM cannot
// run in a transaction, so it uses one dedicated pooled connection and lets
// SQLite acquire its required exclusive database lock for the statement.
func (s *Store) VacuumContext(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return ErrGCUnsupported
	}
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire SQLite connection for VACUUM: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("vacuum SQLite archive: %w", err)
	}
	return nil
}
