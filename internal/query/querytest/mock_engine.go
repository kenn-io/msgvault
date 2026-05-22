// Package querytest provides shared test doubles for the query.Engine interface.
package querytest

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

// MockEngine implements query.Engine for testing. Each method delegates to an
// optional function field; when the field is nil, a safe zero value is returned.
type MockEngine struct {
	SearchFastResults []query.MessageSummary
	SearchResults     []query.MessageSummary
	ListResults       []query.MessageSummary
	Messages          map[int64]*query.MessageDetail
	Attachments       map[int64]*query.AttachmentInfo
	Stats             *query.TotalStats
	Accounts          []query.AccountInfo
	AggregateRows     []query.AggregateRow
	GmailIDs          []string

	// MessagesBySourceID maps source IDs to message details for GetMessageBySourceID.
	// When nil, GetMessageBySourceID falls back to scanning Messages for a matching SourceMessageID.
	MessagesBySourceID map[string]*query.MessageDetail

	// Optional overrides — set these to customise behavior per-test.
	SearchFastFunc               func(context.Context, *search.Query, query.MessageFilter, int, int) ([]query.MessageSummary, error)
	SearchFunc                   func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error)
	GetMessageFunc               func(context.Context, int64) (*query.MessageDetail, error)
	GetMessageBySourceIDFunc     func(context.Context, string) (*query.MessageDetail, error)
	GetTotalStatsFunc            func(context.Context, query.StatsOptions) (*query.TotalStats, error)
	ListMessagesFunc             func(context.Context, query.MessageFilter) ([]query.MessageSummary, error)
	SearchFastCountFunc          func(context.Context, *search.Query, query.MessageFilter) (int64, error)
	GetGmailIDsByFilterFunc      func(context.Context, query.MessageFilter) ([]string, error)
	SearchByDomainsFunc          func(context.Context, []string, *time.Time, *time.Time, int, int) ([]query.MessageSummary, error)
	SearchFastWithStatsFunc      func(context.Context, *search.Query, string, query.MessageFilter, query.ViewType, int, int) (*query.SearchFastResult, error)
	GetMessageRawFunc            func(context.Context, int64) ([]byte, error)
	GetMessageSummariesByIDsFunc func(context.Context, []int64) ([]query.MessageSummary, error)

	RawMessages map[int64][]byte
}

// Compile-time check.
var _ query.Engine = (*MockEngine)(nil)

func (m *MockEngine) Aggregate(_ context.Context, _ query.ViewType, _ query.AggregateOptions) ([]query.AggregateRow, error) {
	return m.AggregateRows, nil
}
func (m *MockEngine) SubAggregate(_ context.Context, _ query.MessageFilter, _ query.ViewType, _ query.AggregateOptions) ([]query.AggregateRow, error) {
	return m.AggregateRows, nil
}

func (m *MockEngine) ListMessages(ctx context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
	if m.ListMessagesFunc != nil {
		return m.ListMessagesFunc(ctx, filter)
	}
	return m.ListResults, nil
}

func (m *MockEngine) GetMessageSummariesByIDs(ctx context.Context, ids []int64) ([]query.MessageSummary, error) {
	if m.GetMessageSummariesByIDsFunc != nil {
		return m.GetMessageSummariesByIDsFunc(ctx, ids)
	}
	// Default: derive summaries from the Messages map (MessageDetail
	// → MessageSummary), preserving caller-supplied order.
	if m.Messages == nil {
		return nil, nil
	}
	out := make([]query.MessageSummary, 0, len(ids))
	for _, id := range ids {
		md, ok := m.Messages[id]
		if !ok || md == nil {
			continue
		}
		out = append(out, query.MessageSummary{
			ID:                   md.ID,
			SourceMessageID:      md.SourceMessageID,
			ConversationID:       md.ConversationID,
			SourceConversationID: md.SourceConversationID,
			Subject:              md.Subject,
			Snippet:              md.Snippet,
			SentAt:               md.SentAt,
			SizeEstimate:         md.SizeEstimate,
			HasAttachments:       md.HasAttachments,
			Labels:               md.Labels,
		})
	}
	return out, nil
}

func (m *MockEngine) GetMessage(ctx context.Context, id int64) (*query.MessageDetail, error) {
	if m.GetMessageFunc != nil {
		return m.GetMessageFunc(ctx, id)
	}
	if m.Messages != nil {
		if msg, ok := m.Messages[id]; ok {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (m *MockEngine) GetMessageBySourceID(ctx context.Context, sourceID string) (*query.MessageDetail, error) {
	if m.GetMessageBySourceIDFunc != nil {
		return m.GetMessageBySourceIDFunc(ctx, sourceID)
	}
	if m.MessagesBySourceID != nil {
		if msg, ok := m.MessagesBySourceID[sourceID]; ok {
			return msg, nil
		}
		return nil, nil
	}
	return nil, nil
}

func (m *MockEngine) GetAttachment(_ context.Context, id int64) (*query.AttachmentInfo, error) {
	if m.Attachments != nil {
		if a, ok := m.Attachments[id]; ok {
			return a, nil
		}
	}
	return nil, nil
}

func (m *MockEngine) GetMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	if m.GetMessageRawFunc != nil {
		return m.GetMessageRawFunc(ctx, id)
	}
	if m.RawMessages != nil {
		if raw, ok := m.RawMessages[id]; ok {
			return raw, nil
		}
	}
	return nil, nil
}

func (m *MockEngine) Search(ctx context.Context, q *search.Query, limit, offset int) ([]query.MessageSummary, error) {
	if m.SearchFunc != nil {
		return m.SearchFunc(ctx, q, limit, offset)
	}
	return m.SearchResults, nil
}

func (m *MockEngine) SearchFast(ctx context.Context, q *search.Query, filter query.MessageFilter, limit, offset int) ([]query.MessageSummary, error) {
	if m.SearchFastFunc != nil {
		return m.SearchFastFunc(ctx, q, filter, limit, offset)
	}
	return m.SearchFastResults, nil
}

func (m *MockEngine) SearchFastCount(ctx context.Context, q *search.Query, filter query.MessageFilter) (int64, error) {
	if m.SearchFastCountFunc != nil {
		return m.SearchFastCountFunc(ctx, q, filter)
	}
	return 0, nil
}

func (m *MockEngine) SearchFastWithStats(ctx context.Context, q *search.Query, queryStr string,
	filter query.MessageFilter, statsGroupBy query.ViewType, limit, offset int) (*query.SearchFastResult, error) {
	if m.SearchFastWithStatsFunc != nil {
		return m.SearchFastWithStatsFunc(ctx, q, queryStr, filter, statsGroupBy, limit, offset)
	}
	return &query.SearchFastResult{
		Messages:   m.SearchFastResults,
		TotalCount: int64(len(m.SearchFastResults)),
		Stats:      m.Stats,
	}, nil
}

func (m *MockEngine) GetGmailIDsByFilter(ctx context.Context, filter query.MessageFilter) ([]string, error) {
	if m.GetGmailIDsByFilterFunc != nil {
		return m.GetGmailIDsByFilterFunc(ctx, filter)
	}
	return m.GmailIDs, nil
}

func (m *MockEngine) SearchByDomains(ctx context.Context, domains []string, after, before *time.Time, limit, offset int) ([]query.MessageSummary, error) {
	if m.SearchByDomainsFunc != nil {
		return m.SearchByDomainsFunc(ctx, domains, after, before, limit, offset)
	}
	return m.SearchResults, nil
}

func (m *MockEngine) ListAccounts(_ context.Context) ([]query.AccountInfo, error) {
	return m.Accounts, nil
}

func (m *MockEngine) GetTotalStats(ctx context.Context, opts query.StatsOptions) (*query.TotalStats, error) {
	if m.GetTotalStatsFunc != nil {
		return m.GetTotalStatsFunc(ctx, opts)
	}
	if m.Stats != nil {
		return m.Stats, nil
	}
	return nil, nil
}

func (m *MockEngine) Close() error { return nil }
