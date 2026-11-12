package cmd

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXDraftReplyUsesManagedChatDestination(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source, messageID := xDraftFixture(t)
	adapter := &storeAPIAdapter{store: st}
	run := func(grant *agentgrant.Grant, args ...string) (string, error) {
		var out strings.Builder
		emit := func(e api.CLIRunEvent) error { out.WriteString(e.Data); return nil }
		req := api.CLIRunRequest{Args: args, Grant: grant}
		var err error
		switch args[0] {
		case "draft-reply":
			err = adapter.runCLIReplyDraft(t.Context(), req, emit)
		case "draft-compose":
			err = adapter.runCLIComposeDraft(t.Context(), req, emit)
		default:
			err = adapter.runCLIDraftLifecycle(t.Context(), req, emit)
		}
		return out.String(), err
	}
	parent := strconv.FormatInt(messageID, 10)
	mailFlags := [][]string{{"--from", "owner@example.com"}, {"--account", "1001"}, {"--source-id", strconv.FormatInt(source.ID, 10)}, {"--all"}}
	body, err := run(nil, "draft-reply", parent, "--body", "owner review", "--json")
	_testRequire.NoError(err)
	var owner chatDraftOutput
	_testRequire.NoError(json.Unmarshal([]byte(body), &owner))
	_testAssert.Equal("msgvault", owner.Location)
	_testAssert.Equal("x", owner.SourceType)
	_testAssert.Equal("102", owner.ReplyToSourceMessageID)
	grant := &agentgrant.Grant{ID: "x-draft-grant", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "x", Identifier: "1001"}}}
	body, err = run(grant, "draft-reply", parent, "--body", "delegated review", "--json")
	_testRequire.NoError(err)
	var created chatDraftOutput
	_testRequire.NoError(json.Unmarshal([]byte(body), &created))
	_, err = run(grant, "draft-get", created.DraftID, "--json")
	_testRequire.EqualError(err, "not_permitted")
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionDraftDelete}
	body, err = run(grant, "draft-get", created.DraftID, "--json")
	_testRequire.NoError(err)
	var metadata chatDraftOutput
	_testRequire.NoError(json.Unmarshal([]byte(body), &metadata))
	_testAssert.Empty(metadata.Body)
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionDraftEdit}
	body, err = run(grant, "draft-get", created.DraftID, "--json")
	_testRequire.NoError(err)
	_testAssert.Contains(body, "delegated review")
	body, err = run(grant, "draft-edit", created.DraftID, "--revision", "1", "--body", "edited review", "--json")
	_testRequire.NoError(err)
	_testAssert.Contains(body, "edited review")
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionDraftDelete}
	_, err = run(grant, "draft-delete", created.DraftID, "--revision", "2", "--json")
	_testRequire.NoError(err)
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionDraftCreate}
	grant.Sources[0].Identifier = "2001"
	_, err = run(grant, "draft-reply", parent, "--body", "wrong source", "--json")
	_testRequire.EqualError(err, "not_permitted")
	for _, flags := range mailFlags {
		args := append([]string{"draft-reply", parent, "--body", "wrong source"}, flags...)
		_, err = run(grant, args...)
		_testRequire.EqualError(err, "not_permitted")
	}
	// Compose is the same local destination and requires an archived parent.
	grant.Sources[0].Identifier = "1001"
	conversation := strconv.FormatInt(owner.ConversationID, 10)
	_, err = run(grant, "draft-compose", "--conversation", conversation, "--body", "without parent", "--json")
	_testRequire.EqualError(err, "invalid_destination")
	body, err = run(grant, "draft-compose", "--conversation", conversation, "--reply-to", parent, "--body", "compose review", "--json")
	_testRequire.NoError(err)
	_testAssert.Contains(body, "compose review")
	for _, flags := range mailFlags {
		args := append([]string{"draft-reply", parent, "--body", "unexpected mail choice"}, flags...)
		_, err = run(nil, args...)
		_testRequire.EqualError(err, "invalid_args")
	}
}

func xDraftFixture(t *testing.T) (*store.Store, *store.Source, int64) {
	t.Helper()

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	saved, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "public parent", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: now}})
	require.NoError(t, err)
	return st, source, saved.MessageID
}

func TestXDraftFrozenAdapterReportsRecoveryCode(t *testing.T) {
	_testRequire := require.New(t)

	st, source, messageID := xDraftFixture(t)
	target, err := st.ResolveXPostContext(t.Context(), source.ID, "100")
	_testRequire.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), target.ConversationID, messageID, "reviewed reply", func(string, string) error { return nil })
	_testRequire.NoError(err)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	_testRequire.NoError(err)
	cost := store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: 5_000_000, CreateReservationMicros: 200_000, At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	admitted, err := st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{SourceID: source.ID, OwnerID: "1001", DraftID: draft.DraftID, Revision: 1, TargetPostID: draft.ReplyToSourceMessageID, Cost: cost})
	_testRequire.NoError(err)
	_testRequire.True(admitted.Created)
	adapter := &storeAPIAdapter{store: st}
	for _, operation := range []string{"draft-edit", "draft-delete"} {
		args := []string{operation, draft.DraftID, "--revision", "1", "--json"}
		if operation == "draft-edit" {
			args = append(args, "--body", "must stay frozen")
		}
		err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args}, func(api.CLIRunEvent) error { return nil })
		_testRequire.EqualError(err, "reply_frozen")
	}
	same, err := st.GetChatDraftContext(t.Context(), draft.DraftID)
	_testRequire.NoError(err)
	assert.Equal(t, draft, same)
}

// Globally invalid syntax is rejected before source lookup, independent of
// source authority. It does not reveal a source-specific validation result.
func TestXDraftInvalidFlagsDoNotAccessArchive(t *testing.T) {
	adapter := &storeAPIAdapter{} // No archive exists at the parser boundary.
	wrongSource := &agentgrant.Grant{ID: "x-draft-grant", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "x", Identifier: "2001"}}}
	for _, grant := range []*agentgrant.Grant{nil, wrongSource} {
		for _, flag := range []string{"--reply-all", "--unknown-draft-flag"} {
			err := adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: []string{"draft-reply", "1", "--body", "synthetic reply", flag}, Grant: grant}, func(api.CLIRunEvent) error { return nil })
			require.EqualError(t, err, "invalid_args")
			var coded *api.CLIRunCodedError
			require.ErrorAs(t, err, &coded)
			require.ErrorContains(t, coded.Err, "unknown flag")
		}
	}
}
