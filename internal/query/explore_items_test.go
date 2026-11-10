package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExploreItemsPopulation(t *testing.T) {
	b := NewTestDataBuilder(t)
	sid := b.AddSource("items@example.com")
	first := b.AddMessage(MessageOpt{SourceID: sid, MessageType: "email", Subject: "Pinned", SentAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)})
	second := b.AddMessage(MessageOpt{SourceID: sid, MessageType: "email", Subject: "Hidden", SentAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})
	b.AddAttachmentWithMIME(91, first, 20, "pinned.pdf", "application/pdf")
	e := b.BuildEngine()
	after := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	req := ExploreRequest{Context: Context{After: &after}, Items: EntryItems{IncludeMessageIDs: []int64{first}, ExcludeMessageIDs: []int64{second}}}
	for _, disabled := range []bool{false, true} {
		e.exploreFastPathDisabled = disabled
		result, err := e.Explore(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, int64(1), result.TotalCount)
		require.Len(t, result.Rows, 1)
		assert.Equal(t, "Pinned", result.Rows[0].Title)
	}
	groups, err := e.ExploreGroups(t.Context(), ExploreGroupRequest{Explore: req, Dimension: "year"})
	require.NoError(t, err)
	require.Len(t, groups.Rows, 1)
	assert.Equal(t, int64(1), groups.Rows[0].Count)
	assert.Equal(t, "2025", groups.Rows[0].Key)
	files, err := e.ExploreFiles(t.Context(), ExploreFilesRequest{Explore: req})
	require.NoError(t, err)
	require.Len(t, files.Files, 1)
	assert.Equal(t, first, files.Files[0].MessageID)
}
func TestExploreEmptyItemsSQLUnchanged(t *testing.T) {
	const base = "source_id = ? AND message_type = ?"
	args := []any{int64(1), "email"}
	got, gotArgs := applyExploreItems(base, args, EntryItems{})
	assert.Equal(t, base, got)
	assert.Equal(t, args, gotArgs)
	request := ExploreRequest{Context: Context{SourceIDs: []int64{1}}, Search: SearchSpec{Mode: SearchFullText, Query: "needle", CandidateMessageIDs: []int64{2, 3}}}
	wantSQL, wantArgs := buildExploreConditions(request)
	gotSQL, gotArgs := buildExploreMatchCountConditions(request)
	assert.Equal(t, wantSQL, gotSQL)
	assert.Equal(t, wantArgs, gotArgs)
}

func TestExploreItemsDistinctExplicitChats(t *testing.T) {
	b := NewTestDataBuilder(t)
	sid := b.AddSourceWithType("chat@example.test", "whatsapp")
	matching := b.AddMessage(MessageOpt{SourceID: sid, MessageType: "whatsapp", ConversationID: 900, ConversationType: "group_chat"})
	b.AddMessage(MessageOpt{SourceID: sid, MessageType: "whatsapp", ConversationID: 900, ConversationType: "group_chat"})
	b.AddMessage(MessageOpt{SourceID: sid, MessageType: "whatsapp", ConversationID: 901, ConversationType: "direct_chat"})
	e := b.BuildEngine()
	after := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, disabled := range []bool{false, true} {
		e.exploreFastPathDisabled = disabled
		included, err := e.Explore(t.Context(), ExploreRequest{Context: Context{After: &after}, Items: EntryItems{IncludeConversationIDs: []int64{900}}})
		require.NoError(t, err)
		require.Len(t, included.Rows, 1)
		assert.Equal(t, "source:1:conversation:900", included.Rows[0].Key)
		assert.Equal(t, int64(2), included.Rows[0].MessageCount)
		excluded, err := e.Explore(t.Context(), ExploreRequest{Items: EntryItems{ExcludeConversationIDs: []int64{900}}})
		require.NoError(t, err)
		require.Len(t, excluded.Rows, 1)
		assert.Equal(t, "source:1:conversation:901", excluded.Rows[0].Key)
	}
	counts, err := e.ExploreMatchCounts(t.Context(), ExploreMatchCountsRequest{
		Explore: ExploreRequest{Search: SearchSpec{Mode: SearchFullText, Query: "needle", CandidateMessageIDs: []int64{matching}, LexicalIndexRevision: "fts5:test"}, Items: EntryItems{IncludeConversationIDs: []int64{900}}},
		RowKeys: []string{"source:1:conversation:900", "source:1:conversation:901"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"source:1:conversation:900": 1, "source:1:conversation:901": 0}, counts.Counts)
}

func TestExploreItemsRespectRequiredTimelineConstraints(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.test")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	b.AddOwnerParticipant(source, owner)
	person := b.AddParticipant("person@person.example", "person.example", "Person")
	alias := b.AddParticipant("alias@person.example", "person.example", "Alias")
	b.LinkCluster(person, alias)
	other := b.AddParticipant("other@other.example", "other.example", "Other")
	ids := make([]int64, 4)
	for i := range ids {
		kind := "email"
		if i == 3 {
			kind = "calendar_event"
		}
		ids[i] = b.AddMessage(MessageOpt{SourceID: source, MessageType: kind, SentAt: time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC)})
	}
	b.AddFrom(ids[0], alias, "Alias")
	b.AddTo(ids[0], owner, "Owner")
	b.AddFrom(ids[1], other, "Other")
	b.AddTo(ids[1], owner, "Owner")
	b.AddFrom(ids[2], owner, "Owner")
	b.AddTo(ids[2], alias, "Alias")
	b.AddFrom(ids[3], alias, "Alias")
	b.AddTo(ids[3], other, "Other")
	b.unsentMessages = fmt.Sprintf("(SELECT %d::BIGINT AS message_id)", ids[2])
	engine := b.BuildEngine()
	after := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, scope := range []Context{{ParticipantIDs: []int64{person}}, {Domains: []string{"person.example"}}} {
		for _, legacy := range []bool{false, true} {
			engine.exploreFastPathDisabled = legacy
			page, err := engine.Explore(t.Context(), ExploreRequest{Context: Context{After: &after}, RequiredContext: &scope, SentOnly: true, PersonInteractionsOnly: true, Items: EntryItems{IncludeMessageIDs: ids}})
			require.NoError(t, err)
			require.Len(t, page.Rows, 1)
			assert.Equal(t, &ids[0], page.Rows[0].AnchorMessageID)
		}
	}
}
