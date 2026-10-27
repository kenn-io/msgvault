package query_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicySearchDefaultAndIncludeCold(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewSQLiteTestStore(t)
	source, err := s.GetOrCreateSource("slack", "synthetic:query")
	requirements.NoError(err)
	ids := []int64{}
	rooms := []int64{}
	for _, tier := range []conversationpolicy.Tier{conversationpolicy.Full, conversationpolicy.Cold} {
		room, err := s.EnsureConversation(source.ID, string(tier), string(tier))
		requirements.NoError(err)
		requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, string(tier), tier))
		id, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: string(tier), MessageType: "slack", Subject: sql.NullString{String: "needle", Valid: true}})
		requirements.NoError(err)
		ids = append(ids, id)
		rooms = append(rooms, room)
	}
	e := query.NewSQLiteEngine(s.DB())
	stats, err := e.GetTotalStats(t.Context(), query.StatsOptions{SearchScope: true})
	requirements.NoError(err)
	assertions.Equal(int64(1), stats.MessageCount)
	listed, err := e.ListMessages(t.Context(), query.MessageFilter{})
	requirements.NoError(err)
	assertions.Len(listed, 1)
	conversations, err := e.ListConversations(t.Context(), query.TextFilter{})
	requirements.NoError(err)
	assertions.Len(conversations, 1)
	coldScope := query.MessageFilter{ConversationID: &rooms[1]}
	coldAggregateStats, err := e.GetTotalStats(t.Context(), query.StatsOptions{Filter: &coldScope, SearchScope: true})
	requirements.NoError(err)
	assertions.Zero(coldAggregateStats.MessageCount)
	coldList, err := e.ListMessages(t.Context(), coldScope)
	requirements.NoError(err)
	assertions.Empty(coldList)
	scopedRows, err := e.SearchFast(t.Context(), search.Parse("subject:needle"), coldScope, 10, 0)
	requirements.NoError(err)
	assertions.Empty(scopedRows)
	scopedRows, err = e.SearchFast(t.Context(), search.Parse("subject:needle include:cold"), coldScope, 10, 0)
	requirements.NoError(err)
	assertions.Len(scopedRows, 1)
	coldRows, err := e.ListConversationMessages(t.Context(), rooms[1], query.TextFilter{})
	requirements.NoError(err)
	assertions.Len(coldRows, 1)
	firstRevision, err := e.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{})
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`UPDATE conversations SET title='Changed cold title' WHERE id=?`), rooms[1])
	requirements.NoError(err)
	secondRevision, err := e.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{})
	requirements.NoError(err)
	assertions.Equal(firstRevision, secondRevision)
	textStats, err := e.GetTextStats(t.Context(), query.TextStatsOptions{})
	requirements.NoError(err)
	assertions.Equal(int64(1), textStats.MessageCount)
	textGroups, err := e.TextAggregate(t.Context(), query.TextViewSources, query.TextAggregateOptions{})
	requirements.NoError(err)
	requirements.Len(textGroups, 1)
	assertions.Equal(int64(1), textGroups[0].Count)
	coldDetail, err := e.GetMessage(t.Context(), ids[1])
	requirements.NoError(err)
	requirements.NotNil(coldDetail)

	for _, include := range []bool{false, true} {
		text := "subject:needle"
		want := ids[:1]
		if include {
			text += " include:cold"
			want = ids
		}
		q := search.ParseOR(text)
		rows, err := e.Search(t.Context(), q, 10, 0)
		requirements.NoError(err)
		assertions.ElementsMatch(want, coldSearchIDs(rows))
		rows, err = e.SearchFast(t.Context(), q, query.MessageFilter{}, 10, 0)
		requirements.NoError(err)
		assertions.ElementsMatch(want, coldSearchIDs(rows))
		page, err := e.SearchFast(t.Context(), q, query.MessageFilter{}, 1, 0)
		requirements.NoError(err)
		assertions.Equal(coldSearchIDs(rows[:1]), coldSearchIDs(page))
		if include {
			page, err = e.SearchFast(t.Context(), q, query.MessageFilter{}, 1, 1)
			requirements.NoError(err)
			assertions.Equal(coldSearchIDs(rows[1:]), coldSearchIDs(page))
		}
		enriched, err := e.SearchFastWithStats(t.Context(), q, text, query.MessageFilter{}, query.ViewSenders, 10, 0)
		requirements.NoError(err)
		requirements.NotNil(enriched.Stats)
		assertions.Equal(int64(len(want)), enriched.Stats.MessageCount)
		assertions.Equal(int64(len(want)), enriched.TotalCount)
		count, err := e.SearchFastCount(t.Context(), q, query.MessageFilter{})
		requirements.NoError(err)
		assertions.Equal(int64(len(want)), count)
	}
	d, err := query.NewDuckDBEngine("", "", s.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, d.Close()) })
	coldQuery := search.Parse("subject:needle include:cold")
	delegated, err := d.Search(t.Context(), coldQuery, 10, 0)
	requirements.NoError(err)
	assertions.Len(delegated, 2)
	delegated, err = d.SearchFast(t.Context(), coldQuery, query.MessageFilter{}, 10, 0)
	requirements.NoError(err)
	assertions.Len(delegated, 2)
	delegatedCount, err := d.SearchFastCount(t.Context(), coldQuery, query.MessageFilter{})
	requirements.NoError(err)
	assertions.Equal(int64(2), delegatedCount)
	delegatedStats, err := d.SearchFastWithStats(t.Context(), coldQuery, "subject:needle include:cold", query.MessageFilter{}, query.ViewNoStats, 10, 0)
	requirements.NoError(err)
	assertions.Len(delegatedStats.Messages, 2)
	assertions.Equal(int64(2), delegatedStats.TotalCount)
	hydrated, err := e.GetMessageSummariesByIDs(t.Context(), ids)
	requirements.NoError(err)
	assertions.Len(hydrated, 1)
	_, err = e.GetDeletionTargetsBySearch(t.Context(), search.Parse("needle include:cold"), query.MessageFilter{}, query.DeletionSearchDeep)
	requirements.ErrorContains(err, "lexical")
}

func TestConversationPolicyAggregatesUseOnlyFullParticipantValues(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("slack", "synthetic:aggregate-policy")
	requirements.NoError(err)
	supportRoom, err := s.EnsureConversation(source.ID, "support", "Synthetic support room")
	requirements.NoError(err)
	targetRoom, err := s.EnsureConversation(source.ID, "target", "Synthetic target room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "support", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "target", conversationpolicy.Full))

	coldOnly, err := s.ResolveConversationParticipantContext(t.Context(), supportRoom, store.ConversationParticipantIdentity{
		IdentifierType: "slack", IdentifierValue: "SYNTHETIC:CONTACT",
		Email: "cold-only@hidden.test", DisplayName: "Cold Only Contact",
	})
	requirements.NoError(err)
	visible, err := s.ResolveConversationParticipantContext(t.Context(), targetRoom, store.ConversationParticipantIdentity{
		IdentifierType: "slack", IdentifierValue: "SYNTHETIC:VISIBLE",
		Email: "visible@example.test", DisplayName: "Visible Contact",
	})
	requirements.NoError(err)
	when := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	message := func(sourceMessageID string, participantID int64) int64 {
		t.Helper()
		id, err := s.UpsertMessage(&store.Message{
			SourceID: source.ID, ConversationID: targetRoom, SourceMessageID: sourceMessageID,
			MessageType: "slack", Subject: sql.NullString{String: sourceMessageID, Valid: true},
			SentAt: sql.NullTime{Time: when, Valid: true},
		})
		requirements.NoError(err)
		requirements.NoError(s.ReplaceMessageRecipientsContext(t.Context(), id, "from", []int64{participantID}, []string{""}))
		requirements.NoError(s.ReplaceMessageRecipientsContext(t.Context(), id, "to", []int64{participantID}, []string{""}))
		return id
	}
	coldOnlyMessage := message("cold-only", coldOnly.ParticipantID)
	message("visible", visible.ParticipantID)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "support", conversationpolicy.Cold))

	e := query.NewEngine(s.DB(), s.IsPostgreSQL())
	for _, view := range []query.ViewType{query.ViewSenders, query.ViewRecipients, query.ViewDomains, query.ViewSenderNames, query.ViewRecipientNames} {
		rows, err := e.Aggregate(t.Context(), view, query.DefaultAggregateOptions())
		requirements.NoError(err)
		keys := make([]string, 0, len(rows))
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
		assertions.NotContains(keys, "cold-only@hidden.test", "view %v must not expose an email supported only by Cold history", view)
		assertions.NotContains(keys, "hidden.test", "view %v must not expose a domain supported only by Cold history", view)
		assertions.NotContains(keys, "Cold Only Contact", "view %v must not expose a name supported only by Cold history", view)
	}

	for _, filter := range []query.MessageFilter{
		{Sender: "cold-only@hidden.test"},
		{Recipient: "cold-only@hidden.test"},
		{SenderName: "Cold Only Contact"},
		{RecipientName: "Cold Only Contact"},
		{Domain: "hidden.test"},
	} {
		rows, err := e.ListMessages(t.Context(), filter)
		requirements.NoError(err)
		assertions.Empty(rows, "filter %+v must not match a Full message using Cold-only participant data", filter)
	}

	searchResults, err := e.SearchByDomains(t.Context(), []string{"hidden.test"}, nil, nil, 20, 0, nil)
	requirements.NoError(err)
	assertions.Empty(searchResults)
	for _, text := range []string{
		"cold-only@hidden.test",
		"Cold Only Contact",
		"from:cold-only@hidden.test",
		"to:cold-only@hidden.test",
	} {
		rows, err := e.SearchFast(t.Context(), search.Parse(text), query.MessageFilter{}, 20, 0)
		requirements.NoError(err)
		assertions.Empty(rows, "search %q must not match a Full message using Cold-only participant data", text)
	}

	hydrated, err := e.GetMessageSummariesByIDs(t.Context(), []int64{coldOnlyMessage})
	requirements.NoError(err)
	requirements.Len(hydrated, 1)
	assertions.Empty(hydrated[0].FromEmail)
	assertions.Empty(hydrated[0].FromName)
	assertions.Empty(hydrated[0].FromPhone)

	all, err := e.ListMessages(t.Context(), query.MessageFilter{})
	requirements.NoError(err)
	var visibleHydrated *query.MessageSummary
	for i := range all {
		if all[i].FromEmail == "visible@example.test" {
			visibleHydrated = &all[i]
		}
	}
	requirements.NotNil(visibleHydrated)
}

func TestConversationPolicySearchDeletionScopeIndependent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("gmail", "archive@example.test")
	requirements.NoError(err)
	ids := map[string]int64{}
	for _, tier := range []string{"full", "cold"} {
		room, err := s.EnsureConversation(source.ID, tier, tier)
		requirements.NoError(err)
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO conversation_policy(conversation_id,effective_tier) VALUES (?,?) ON CONFLICT(conversation_id) DO UPDATE SET effective_tier=excluded.effective_tier`), room, tier)
		requirements.NoError(err)
		ids[tier], err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: tier, MessageType: "email", Subject: sql.NullString{String: "needle", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
		requirements.NoError(err)
	}
	e := query.NewEngine(s.DB(), s.IsPostgreSQL())
	searchResolver, ok := e.(query.DeletionTargetSearchResolver)
	requirements.True(ok)
	aggregateResolver, ok := e.(query.DeletionTargetAggregateSearchResolver)
	requirements.True(ok)
	idResolver, ok := e.(interface {
		GetDeletionTargetsByMessageIDs(ctx context.Context, messageIDs []int64) ([]query.DeletionTarget, error)
	})
	requirements.True(ok)
	filter := query.MessageFilter{SourceID: &source.ID}
	parsed := search.Parse("subject:needle")
	pages, err := e.SearchFast(t.Context(), parsed, filter, 20, 0)
	requirements.NoError(err)
	wantIDs := []int64{ids["full"]}
	assertions.Equal(wantIDs, coldSearchIDs(pages))
	count, err := e.SearchFastCount(t.Context(), parsed, filter)
	requirements.NoError(err)
	assertions.Equal(int64(len(wantIDs)), count)
	coldQuery := search.Parse("subject:needle include:cold")
	coldPages, err := e.SearchFast(t.Context(), coldQuery, filter, 20, 0)
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{ids["full"], ids["cold"]}, coldSearchIDs(coldPages))
	coldCount, err := e.SearchFastCount(t.Context(), coldQuery, filter)
	requirements.NoError(err)
	assertions.Equal(int64(2), coldCount)
	hydrated, err := e.GetMessageSummariesByIDs(t.Context(), []int64{ids["full"], ids["cold"]})
	requirements.NoError(err)
	assertions.Equal(wantIDs, coldSearchIDs(hydrated))
	for _, mode := range []query.DeletionSearchMode{query.DeletionSearchFast, query.DeletionSearchDeep} {
		targets, err := searchResolver.GetDeletionTargetsBySearch(t.Context(), parsed, filter, mode)
		requirements.NoError(err)
		assertions.Equal(wantIDs, coldDeletionIDs(targets))
	}
	filtered, err := e.ListMessages(t.Context(), filter)
	requirements.NoError(err)
	targets, err := e.GetDeletionTargetsByFilter(t.Context(), filter)
	requirements.NoError(err)
	assertions.Equal(coldSearchIDs(filtered), coldDeletionIDs(targets))
	opts := query.DefaultAggregateOptions()
	opts.SearchQuery = "subject:needle"
	opts.TimeGranularity = query.TimeMonth
	groups, err := e.SubAggregate(t.Context(), filter, query.ViewTime, opts)
	requirements.NoError(err)
	requirements.Len(groups, 1)
	assertions.Equal(int64(len(wantIDs)), groups[0].Count)
	targets, err = aggregateResolver.GetDeletionTargetsByAggregateSearch(t.Context(), "subject:needle", filter, query.ViewTime, groups[0].Key)
	requirements.NoError(err)
	assertions.Equal(wantIDs, coldDeletionIDs(targets))
	explicit, err := idResolver.GetDeletionTargetsByMessageIDs(t.Context(), []int64{ids["cold"]})
	requirements.NoError(err)
	assertions.Equal([]int64{ids["cold"]}, coldDeletionIDs(explicit))
}

func coldDeletionIDs(rows []query.DeletionTarget) []int64 {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.MessageID
	}
	return ids
}

func coldSearchIDs(rows []query.MessageSummary) []int64 {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}
