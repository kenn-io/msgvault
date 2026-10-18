package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicySearchDefaultAndIncludeCold(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("slack", "synthetic:search")
	requirements.NoError(err)
	ids := map[string]int64{}
	for _, tier := range []string{"full", "cold", "stub", "skip", "missing"} {
		room, err := s.EnsureConversation(source.ID, tier, tier)
		requirements.NoError(err)
		switch tier {
		case "full", "cold":
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, tier, conversationpolicy.Tier(tier)))
		case "missing":
			_, err = s.DB().Exec(s.Rebind(`DELETE FROM conversation_policy WHERE conversation_id=?`), room)
			requirements.NoError(err)
		default:
			_, err = s.DB().Exec(s.Rebind(`UPDATE conversation_policy SET effective_tier=? WHERE conversation_id=?`), tier, room)
			requirements.NoError(err)
		}
		ids[tier], err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: tier, MessageType: "slack", Subject: sql.NullString{String: "needle", Valid: true}})
		requirements.NoError(err)
		requirements.NoError(s.UpsertMessageBody(ids[tier], sql.NullString{String: "bodyneedle", Valid: true}, sql.NullString{}))
		requirements.NoError(s.UpsertFTS(ids[tier], "needle", "bodyneedle", "", "", ""))
	}
	listed, listTotal, err := s.ListMessagesContext(t.Context(), 0, 20, nil)
	requirements.NoError(err)
	assertions.Len(listed, 1)
	assertions.Equal(int64(1), listTotal)
	summaries, err := s.GetMessagesSummariesByIDsContext(t.Context(), []int64{ids["full"], ids["cold"]})
	requirements.NoError(err)
	assertions.Len(summaries, 1)
	detail, err := s.GetMessageContext(t.Context(), ids["cold"])
	requirements.NoError(err)
	requirements.NotNil(detail)
	assertions.Equal("bodyneedle", detail.BodyText)
	metadata, err := s.GetSearchMetadata(t.Context(), []int64{ids["full"], ids["cold"]}, search.Parse("subject:needle"), false)
	requirements.NoError(err)
	assertions.Len(metadata, 1)
	metadata, err = s.GetSearchMetadata(t.Context(), []int64{ids["full"], ids["cold"]}, search.Parse("subject:needle include:cold"), false)
	requirements.NoError(err)
	assertions.Len(metadata, 2)

	deniedIDs, deniedTotal, err := s.SearchMessageIDsQueryContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), search.ParseOR("subject:needle | bodyneedle include:cold"), 20)
	requirements.NoError(err)
	assertions.Empty(deniedIDs)
	assertions.Zero(deniedTotal)

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"subject:needle", []int64{ids["full"]}},
		{"bodyneedle", []int64{ids["full"]}},
		{"bodyneedle include:cold", []int64{ids["full"], ids["cold"]}},
		{"subject:needle include:cold", []int64{ids["full"], ids["cold"]}},
		{"subject:needle | subject:absent", []int64{ids["full"]}},
		{"subject:needle | subject:absent include:cold", []int64{ids["full"], ids["cold"]}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			got, count, err := s.SearchMessageIDsQueryContext(t.Context(), search.ParseOR(tc.query), 20)
			requirements.NoError(err)
			assertions.ElementsMatch(tc.want, got)
			assertions.Equal(int64(len(tc.want)), count)
			messages, total, err := s.SearchMessagesQueryContext(t.Context(), search.ParseOR(tc.query), 0, 20)
			requirements.NoError(err)
			assertions.Len(messages, len(tc.want))
			assertions.Equal(count, total)
		})
	}
}

func TestConversationPolicySearchDeletionScopeIndependent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("slack", "synthetic:deleted")
	requirements.NoError(err)
	ids := map[string]int64{}
	for _, tier := range []conversationpolicy.Tier{conversationpolicy.Full, conversationpolicy.Cold} {
		room, err := s.EnsureConversation(source.ID, string(tier), string(tier))
		requirements.NoError(err)
		requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, string(tier), tier))
		for _, deleted := range []bool{false, true} {
			key := fmt.Sprintf("%s-%t", tier, deleted)
			id, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: key, MessageType: "slack", Subject: sql.NullString{String: "needle", Valid: true}})
			requirements.NoError(err)
			if deleted {
				_, err = s.DB().Exec(s.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), id)
				requirements.NoError(err)
			}
			ids[key] = id
		}
	}
	for _, include := range []bool{false, true} {
		for _, scope := range []search.DeletionScope{search.DeletionScopeActive, search.DeletionScopeDeleted, search.DeletionScopeAny} {
			q := search.Parse("subject:needle")
			q.IncludeCold = include
			q.DeletionScope = scope
			got, _, err := s.SearchMessageIDsQueryContext(t.Context(), q, 20)
			requirements.NoError(err)
			want := []int64{}
			for _, tier := range []string{"full", "cold"} {
				if tier == "cold" && !include {
					continue
				}
				for _, deleted := range []bool{false, true} {
					if (scope == search.DeletionScopeActive && deleted) || (scope == search.DeletionScopeDeleted && !deleted) {
						continue
					}
					want = append(want, ids[fmt.Sprintf("%s-%t", tier, deleted)])
				}
			}
			assertions.ElementsMatch(want, got)
		}
	}
	source, err = s.GetOrCreateSource("custom-chat", "synthetic:unsupported")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "unsupported", "Unsupported")
	requirements.NoError(err)
	id, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "unsupported", Subject: sql.NullString{String: "unsupported", Valid: true}})
	requirements.NoError(err)
	got, _, err := s.SearchMessageIDsQueryContext(t.Context(), search.Parse("subject:unsupported"), 20)
	requirements.NoError(err)
	assertions.Equal([]int64{id}, got)
}
