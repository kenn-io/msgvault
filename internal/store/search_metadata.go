package store

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/textutil"
)

// SearchMetadata enriches a bounded result page without reading message bodies.
type SearchMetadata struct {
	AttachmentNames []string
	AttachmentCount int
	MatchSnippet    string
}

func (s *Store) GetSearchMetadata(ctx context.Context, ids []int64, q *search.Query, includeSnippet bool) (map[int64]SearchMetadata, error) {
	result := make(map[int64]SearchMetadata, len(ids))
	// Attachment rows can lag the authoritative stored count while sync is writing them.
	err := queryInChunksContext(ctx, s.db, ids, nil, `SELECT m.id, COALESCE(m.attachment_count, 0), a.id IS NOT NULL, COALESCE(a.filename, '')
		FROM messages m LEFT JOIN attachments a ON a.message_id = m.id
		WHERE m.id IN (%s) ORDER BY m.id, a.id`, func(rows *loggedRows) error {
		var id int64
		var count int
		var hasRow bool
		var name string
		if err := rows.Scan(&id, &count, &hasRow, &name); err != nil {
			return err
		}
		item, ok := result[id]
		if !ok {
			item.AttachmentNames = []string{}
		}
		item.AttachmentCount = count
		if hasRow && name != "" {
			item.AttachmentNames = append(item.AttachmentNames, name)
		}
		result[id] = item
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search attachment metadata: %w", err)
	}
	if !includeSnippet || q == nil || len(q.TextTerms) == 0 || s.IsPostgreSQL() || !s.FTS5Available() {
		return result, nil
	}
	expression := s.dialect.BuildFTSArg(q.TextTerms)
	if expression == "" {
		return result, nil
	}
	err = queryInChunksContext(ctx, s.db, ids, []any{expression}, `SELECT rowid, snippet(messages_fts, -1, char(1), char(2), '…', 24)
		FROM messages_fts WHERE messages_fts MATCH ? AND rowid IN (%s)`, func(rows *loggedRows) error {
		var id int64
		var excerpt string
		if err := rows.Scan(&id, &excerpt); err != nil {
			return err
		}
		if item, ok := result[id]; ok {
			item.MatchSnippet = boundedMatchSnippet(excerpt)
			result[id] = item
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("search match context: %w", ctx.Err())
	}
	// Optional excerpts can fall back to previews without losing attachment metadata.
	return result, nil
}

func boundedMatchSnippet(text string) string {
	runes := []rune(text)
	start := 0
	for i, r := range runes {
		if r == 1 {
			if i > 80 {
				start = max(0, i-4)
			}
			break
		}
	}
	if start > 0 {
		runes = append([]rune{'…'}, runes[start:]...)
	}
	cleaned := strings.Map(func(r rune) rune {
		if r == 1 || r == 2 {
			return -1
		}
		return r
	}, string(runes))
	if short := textutil.PrefixRunes(cleaned, 160); short != cleaned {
		return short + "…"
	}
	return cleaned
}
