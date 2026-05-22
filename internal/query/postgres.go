// Package query - PostgreSQL query engine implementation.
//
// PostgreSQLEngine implements the Engine interface for PostgreSQL backends.
// It parallels SQLiteEngine but uses PostgreSQL-specific SQL:
//   - $N placeholders instead of ?
//   - to_char() instead of strftime()
//   - information_schema instead of sqlite_master
//   - tsvector/@@/ts_rank instead of FTS5 MATCH/rank
//
// This implementation is scaffolded to compile and handle the simpler
// read paths (GetAttachment, ListAccounts, GetTotalStats without search).
// Methods whose SQLite versions depend on FTS5, the shared query builder,
// or multi-table assembly (GetMessage*, ListMessages, Aggregate, Search*,
// GetGmailIDsByFilter) return ErrNotImplemented pending follow-up work to
// parameterize the shared query builders.
package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/search"
)

// ErrNotImplemented is returned by PostgreSQLEngine methods that require
// query-builder parameterization not yet complete. Tracked for follow-up.
var ErrNotImplemented = errors.New("PostgreSQLEngine: method not yet implemented")

// PostgreSQLEngine implements Engine using direct PostgreSQL queries via pgx.
type PostgreSQLEngine struct {
	db *sql.DB
}

// NewPostgreSQLEngine creates a new PostgreSQL-backed query engine.
func NewPostgreSQLEngine(db *sql.DB) *PostgreSQLEngine {
	return &PostgreSQLEngine{db: db}
}

// Close is a no-op since PostgreSQLEngine doesn't own the connection.
func (e *PostgreSQLEngine) Close() error {
	return nil
}

// Aggregate performs grouping based on the provided ViewType.
// Requires dialect-aware time expression handling; see ErrNotImplemented.
func (e *PostgreSQLEngine) Aggregate(ctx context.Context, groupBy ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	return nil, ErrNotImplemented
}

// SubAggregate performs aggregation on a filtered subset of messages.
func (e *PostgreSQLEngine) SubAggregate(ctx context.Context, filter MessageFilter, groupBy ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	return nil, ErrNotImplemented
}

// ListMessages returns messages matching the filter.
func (e *PostgreSQLEngine) ListMessages(ctx context.Context, filter MessageFilter) ([]MessageSummary, error) {
	return nil, ErrNotImplemented
}

// GetMessageSummariesByIDs returns summary-level rows for the supplied IDs.
func (e *PostgreSQLEngine) GetMessageSummariesByIDs(ctx context.Context, ids []int64) ([]MessageSummary, error) {
	return nil, ErrNotImplemented
}

// GetMessage retrieves a full message by internal ID.
//
// Returns ErrNotImplemented: the SQLite path also fetches participants,
// labels, attachments, ReceivedAt, and a raw-MIME body fallback. Returning
// a partially-populated MessageDetail would silently violate the Engine
// contract for callers, so the partial implementation is held back until
// the full PostgreSQL message-detail path lands.
func (e *PostgreSQLEngine) GetMessage(ctx context.Context, id int64) (*MessageDetail, error) {
	return nil, ErrNotImplemented
}

// GetMessageBySourceID retrieves a full message by source message ID.
//
// Returns ErrNotImplemented for the same reasons as GetMessage.
func (e *PostgreSQLEngine) GetMessageBySourceID(ctx context.Context, sourceMessageID string) (*MessageDetail, error) {
	return nil, ErrNotImplemented
}

// GetMessageRaw returns the decompressed raw MIME data for a message.
//
// Returns ErrNotImplemented: the PostgreSQL store does not yet populate
// message_raw, so there is nothing to read. Wired up alongside the rest of
// the message-detail path.
func (e *PostgreSQLEngine) GetMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	return nil, ErrNotImplemented
}

// GetAttachment retrieves attachment metadata by ID.
func (e *PostgreSQLEngine) GetAttachment(ctx context.Context, id int64) (*AttachmentInfo, error) {
	var att AttachmentInfo
	err := e.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(filename, ''), COALESCE(mime_type, ''), COALESCE(size, 0), COALESCE(content_hash, '')
		FROM attachments
		WHERE id = $1
	`, id).Scan(&att.ID, &att.Filename, &att.MimeType, &att.Size, &att.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get attachment: %w", err)
	}
	return &att, nil
}

// Search performs a Gmail-style search query using tsvector FTS.
func (e *PostgreSQLEngine) Search(ctx context.Context, query *search.Query, limit, offset int) ([]MessageSummary, error) {
	return nil, ErrNotImplemented
}

// SearchFast searches message metadata only (no body text).
func (e *PostgreSQLEngine) SearchFast(ctx context.Context, query *search.Query, filter MessageFilter, limit, offset int) ([]MessageSummary, error) {
	return nil, ErrNotImplemented
}

// SearchFastCount returns the total count of messages matching a search query.
func (e *PostgreSQLEngine) SearchFastCount(ctx context.Context, query *search.Query, filter MessageFilter) (int64, error) {
	return 0, ErrNotImplemented
}

// SearchFastWithStats performs a fast metadata search with stats.
func (e *PostgreSQLEngine) SearchFastWithStats(ctx context.Context, query *search.Query, queryStr string,
	filter MessageFilter, statsGroupBy ViewType, limit, offset int) (*SearchFastResult, error) {
	return nil, ErrNotImplemented
}

// GetGmailIDsByFilter returns Gmail message IDs matching a filter.
func (e *PostgreSQLEngine) GetGmailIDsByFilter(ctx context.Context, filter MessageFilter) ([]string, error) {
	return nil, ErrNotImplemented
}

// SearchByDomains returns messages where any participant matches one of the given domains.
func (e *PostgreSQLEngine) SearchByDomains(ctx context.Context, domains []string, after, before *time.Time, limit, offset int) ([]MessageSummary, error) {
	return nil, ErrNotImplemented
}

// ListAccounts returns all source accounts.
func (e *PostgreSQLEngine) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, source_type, identifier, COALESCE(display_name, '')
		FROM sources
		ORDER BY identifier
	`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var accounts []AccountInfo
	for rows.Next() {
		var acc AccountInfo
		if err := rows.Scan(&acc.ID, &acc.SourceType, &acc.Identifier, &acc.DisplayName); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, acc)
	}
	return accounts, rows.Err()
}

// GetTotalStats returns overall statistics.
func (e *PostgreSQLEngine) GetTotalStats(ctx context.Context, opts StatsOptions) (*TotalStats, error) {
	stats := &TotalStats{}

	// Basic stats without search filtering (search filtering TBD with query builder parameterization)
	if opts.SearchQuery != "" {
		return nil, ErrNotImplemented
	}

	whereClause, args := postgresStatsWhereClause(opts)

	// Message stats
	msgQuery := fmt.Sprintf(`SELECT COUNT(*), COALESCE(SUM(m.size_estimate), 0) FROM messages m WHERE %s`,
		whereClause)
	if err := e.db.QueryRowContext(ctx, msgQuery, args...).Scan(&stats.MessageCount, &stats.TotalSize); err != nil {
		return nil, fmt.Errorf("message stats: %w", err)
	}

	// Attachment stats — join messages and apply the same filters so that
	// counts and sizes don't include attachments for soft-deleted messages,
	// other accounts, or other filtered-out rows.
	attQuery := fmt.Sprintf(`SELECT COUNT(*), COALESCE(SUM(a.size), 0)
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE %s`, whereClause)
	if err := e.db.QueryRowContext(ctx, attQuery, args...).Scan(&stats.AttachmentCount, &stats.AttachmentSize); err != nil {
		return nil, fmt.Errorf("attachment stats: %w", err)
	}

	// Label count
	if opts.SourceID != nil {
		if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM labels WHERE source_id = $1", *opts.SourceID).Scan(&stats.LabelCount); err != nil {
			return nil, fmt.Errorf("label count: %w", err)
		}
	} else {
		if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM labels").Scan(&stats.LabelCount); err != nil {
			return nil, fmt.Errorf("label count: %w", err)
		}
	}

	// Account count
	if opts.SourceID != nil {
		if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sources WHERE id = $1", *opts.SourceID).Scan(&stats.AccountCount); err != nil {
			return nil, fmt.Errorf("account count: %w", err)
		}
	} else {
		if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sources").Scan(&stats.AccountCount); err != nil {
			return nil, fmt.Errorf("account count: %w", err)
		}
	}

	return stats, nil
}

func postgresStatsWhereClause(opts StatsOptions) (string, []interface{}) {
	msgConds := []string{emailOnlyFilterM}
	var args []interface{}
	if opts.SourceID != nil {
		msgConds = append(msgConds, fmt.Sprintf("m.source_id = $%d", len(args)+1))
		args = append(args, *opts.SourceID)
	}
	if opts.WithAttachmentsOnly {
		msgConds = append(msgConds, "m.has_attachments = TRUE")
	}
	if opts.HideDeletedFromSource {
		msgConds = append(msgConds, "m.deleted_from_source_at IS NULL")
	}
	return strings.Join(msgConds, " AND "), args
}
