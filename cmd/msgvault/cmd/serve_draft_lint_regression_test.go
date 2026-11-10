package cmd

import (
	"bytes"
	"context"
	"log/slog"
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
	id := lintObject(t, events)["draft_id"].(string)
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
	assertDraftRows(t, st, 0)
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
	assert.Nil(t, got["content"])
	assert.NotContains(t, got, "candidate_content")
	assert.NotContains(t, got, "pending_operation")
	after, err := f.store.GetBeeperDraftContext(t.Context(), draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, draft, after)
	requests, patches := f.beeper.counts()
	assert.Zero(t, requests)
	assert.Zero(t, patches)
}

func TestDraftLintNameLookupDiagnostics(t *testing.T) {
	readGrant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageRead}}
	writeGrant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}
	for _, tc := range []struct {
		name, body string
		grant      *agentgrant.Grant
		skip, log  bool
	}{
		{"source read failure", "Hi Alex,", readGrant, true, true},
		{"name read failure", "Hi Alex,", nil, true, true},
		{"write only", "Hi Alex,", writeGrant, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			// A closed archive makes a real dependency failure deterministic.
			require.NoError(t, st.Close())
			var logs bytes.Buffer
			a := &storeAPIAdapter{store: st, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			in := draftlint.Input{Kind: "reply", Channel: "email", Body: tc.body, To: []string{"alex@example.com"}}
			result := a.lintDraft(t.Context(), in, 1, tc.grant)
			assert.Empty(t, result.Warnings)
			if tc.skip {
				assert.Equal(t, []string{draftlint.CodeGreetingNameMismatch}, result.Skipped)
			} else {
				assert.Empty(t, result.Skipped)
			}
			if tc.log {
				assert.Contains(t, logs.String(), `"msg":"draft greeting name lookup failed"`)
				assert.Contains(t, logs.String(), `"error_class":"archive_read_failed"`)
				assert.NotContains(t, logs.String(), "alex@example.com")
				assert.NotContains(t, logs.String(), "database is closed")
				// Nil-logger adapters still return an advisory skip on failure.
				a.logger = nil
				assert.Equal(t, result, a.lintDraft(t.Context(), in, 1, tc.grant))
			} else {
				assert.Empty(t, logs.String())
			}
		})
	}
}

func TestDraftLintCancelledGate(t *testing.T) {
	st := testutil.NewTestStore(t)
	a := &storeAPIAdapter{store: st}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, options := range []draftLintOptions{{ValidateOnly: true}, {Strict: true}, {}} {
		var events []api.CLIRunEvent
		_, done, err := a.gateDraftLint(ctx, draftlint.Input{Kind: "reply", Channel: "email", Body: "Hi Alex,", To: []string{"alex@example.com"}}, 1, nil, options, true, func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		})
		require.EqualError(t, err, "cancelled")
		assert.True(t, done)
		assert.Empty(t, events)
	}
}
