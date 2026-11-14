package cmd

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/draftlint"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftForwardEmptyNoteEdit(t *testing.T) {
	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	events, err := lintRun(t, a, nil, "draft-forward", strconv.FormatInt(f.parentID, 10), "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "alex@example.com", "--body", "original", "--json")
	require.NoError(t, err)
	id, ok := lintObject(t, events)["draft_id"].(string)
	require.True(t, ok)
	args := []string{"draft-edit", id, "--revision", "1", "--body", "", "--strict", "--json"}
	_, err = lintRun(t, a, nil, args...)
	require.NoError(t, err)
	after, err := f.store.GetIMAPDraftContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), after.Revision)
}
func TestDraftChatAuthorizedInvalidReply(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic")
	require.NoError(t, err)
	conversation, err := st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
	require.NoError(t, err)
	a := &storeAPIAdapter{store: st}
	for _, scoped := range []bool{true, false} {
		grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}}
		want := "invalid_destination"
		if !scoped {
			grant.Sources[0].Identifier = "other"
			want = "not_permitted"
		}
		_, err := lintRun(t, a, grant, "draft-compose", "--conversation", strconv.FormatInt(conversation, 10), "--reply-to", "99999", "--body", "Thanks", "--validate-only")
		require.EqualError(t, err, want)
	}
	assertDraftRows(t, st)
}
func TestDraftBeeperValidateLocalDuplicate(t *testing.T) {
	f := newBeeperDraftFixture(t)
	draft, err := f.store.CreateBeeperDraftContext(t.Context(), f.source.ID, beeperDraftTestChat, "private draft")
	require.NoError(t, err)
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: f.source.ID, Type: f.source.SourceType, Identifier: f.source.Identifier}}}
	events, err := lintRun(t, f.adapter, grant, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "  "+beeperDraftTestChat+"  ", "--body", "Thanks", "--validate-only", "--json")
	require.EqualError(t, err, "draft_exists")
	got := lintObject(t, events)
	assert.Equal(t, draft.DraftID, got["draft_id"])
	after, err := f.store.GetBeeperDraftContext(t.Context(), draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, draft, after)
	requests, patches := f.beeper.counts()
	assert.Zero(t, requests)
	assert.Zero(t, patches)
}

func TestDraftLintCancelledGate(t *testing.T) {
	st := testutil.NewTestStore(t)
	a := &storeAPIAdapter{store: st}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, options := range []draftLintOptions{{ValidateOnly: true}, {}} {
		var events []api.CLIRunEvent
		_, done, err := a.gateDraftLint(ctx, draftlint.Input{Kind: "reply", Channel: "email", Body: "Hi Alex,", To: []string{"alex@example.com"}}, options, true, func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		})
		require.EqualError(t, err, "cancelled")
		assert.True(t, done)
		assert.Empty(t, events)
	}
}
