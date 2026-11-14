package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/draftlint"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func lintRun(t *testing.T, a *storeAPIAdapter, grant *agentgrant.Grant, args ...string) ([]api.CLIRunEvent, error) {
	t.Helper()
	var events []api.CLIRunEvent
	req := api.CLIRunRequest{Args: args, Grant: grant}
	//nolint:unparam // The event callback contract requires an error return.
	var emit = func(e api.CLIRunEvent) error { events = append(events, e); return nil }
	switch args[0] {
	case "draft-reply":
		return eventsWithError(&events, func() error { return a.runCLIReplyDraft(t.Context(), req, emit) })
	case "draft-compose":
		return eventsWithError(&events, func() error { return a.runCLIComposeDraft(t.Context(), req, emit) })
	case "draft-forward":
		return eventsWithError(&events, func() error { return a.runCLIForwardDraft(t.Context(), req, emit) })
	default:
		return eventsWithError(&events, func() error { return a.runCLIDraftLifecycle(t.Context(), req, emit) })
	}
}
func eventsWithError(events *[]api.CLIRunEvent, run func() error) ([]api.CLIRunEvent, error) {
	err := run()
	return *events, err
}
func lintObject(t *testing.T, events []api.CLIRunEvent) map[string]any {
	t.Helper()
	require.NotEmpty(t, events)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &got))
	return got
}
func assertDraftRows(t *testing.T, st *store.Store) {
	t.Helper()
	for _, table := range []string{"imap_drafts", "gmail_drafts", "chat_drafts", "beeper_drafts", "attachments"} {
		var n int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n))
		assert.Zero(t, n, table)
	}
}
func TestDraftReplyStrictRefuses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	calls := 0
	a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		calls++
		return nil, errors.New("unexpected provider call")
	}
	args := []string{"draft-reply", strconv.FormatInt(f.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", "Hi FIRST_NAME", "--strict"}
	events, err := lintRun(t, a, nil, append(args, "--json")...)
	require.EqualError(err, "draft_lint_failed")
	got := lintObject(t, events)
	assert.Equal("refused", got["status"])
	assert.Equal(cliStreamStderr, events[0].Type)
	events, err = lintRun(t, a, nil, args...)
	require.EqualError(err, "draft_lint_failed")
	require.NotEmpty(events)
	assert.Contains(events[0].Data, "Refused.")
	assert.Contains(events[0].Data, "error: The text contains a template placeholder.")
	for _, code := range []string{"draft_lint_failed", "unresolved_placeholder"} {
		assert.NotContains(events[0].Data, code)
	}
	assert.NotContains(events[0].Data, `{"`)
	args = []string{"draft-forward", strconv.FormatInt(f.parentID, 10), "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "a@example.com", "--body", "FIRST_NAME", "--json"}
	events, err = lintRun(t, a, nil, append(args, "--validate-only")...)
	require.NoError(err)
	assert.Equal(false, lintObject(t, events)["valid"])
	_, err = lintRun(t, a, nil, append(args, "--strict")...)
	require.EqualError(err, "draft_lint_failed")
	assert.Zero(calls)
	assertDraftRows(t, f.store)
}

func TestDraftLintRepeatedFindingsOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	in := draftlint.Input{Channel: "chat", Body: "FIRST_NAME LAST_NAME <URL> <URL>"}
	result := draftlint.Check(in)
	var events []api.CLIRunEvent
	emit := func(event api.CLIRunEvent) error { events = append(events, event); return nil }
	require.NoError(emitValidatedDraft(emit, result, in, false))
	require.Len(events, 1)
	assert.Equal(1, strings.Count(events[0].Data, "error: The text contains a template placeholder."))
	assert.Equal(1, strings.Count(events[0].Data, "warn: The text contains a template placeholder."))
	events = nil
	require.NoError(emitValidatedDraft(emit, result, in, true))
	got := lintObject(t, events)
	warnings, ok := got["warnings"].([]any)
	require.True(ok)
	assert.Len(warnings, 4)
}

func TestDraftCreateLocalFailureReportsLint(t *testing.T) {
	for _, provider := range []string{"imap", "gmail"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", provider, asJSON), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				var events []api.CLIRunEvent
				var err error
				if provider == "imap" {
					f := newDraftReplyFixture(t)
					conversation, createErr := f.store.EnsureConversation(t.Context(), f.source.ID, "stale", "Stale")
					require.NoError(createErr)
					_, createErr = f.store.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: f.source.ID, SourceMessageID: "Drafts|1", ConversationID: conversation, MessageType: store.MessageTypeEmail, Subject: sql.NullString{String: "Stale", Valid: true}}, BodyText: sql.NullString{String: "stale", Valid: true}, RawMIME: []byte("Subject: Stale\r\n\r\nstale\r\n")})
					require.NoError(createErr)
					args := []string{"--body", "FIRST_NAME"}
					if asJSON {
						args = append(args, "--json")
					}
					events, err = f.run(t, f.grantedAdapter(), args...)
					require.EqualError(err, "remote_accepted_local_failed")
				} else {
					f := newSQLiteGmailDraftTestFixture(t)
					_, createErr := f.store.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_gmail_create BEFORE INSERT ON gmail_drafts BEGIN SELECT RAISE(FAIL, 'injected failure'); END`)
					require.NoError(createErr)
					events, err = f.create(t, "FIRST_NAME", asJSON)
					require.EqualError(err, "remote_accepted_local_failed")
					assert.Equal(1, f.client.createCalls)
				}
				require.Len(events, 1)
				assert.Equal(cliStreamStderr, events[0].Type)
				if asJSON {
					assert.Contains(events[0].Data, `"code":"unresolved_placeholder"`)
				} else {
					assert.Contains(events[0].Data, "error: The text contains a template placeholder.")
				}
			})
		}
	}
}
func TestDraftReplyLintAndProvenanceWarnings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftReplyFixture(t)
	events, err := f.run(t, f.grantedAdapter(), "--body", "Hi FIRST_NAME", "--json")
	require.NoError(err)
	warnings, ok := lintObject(t, events)["warnings"].([]any)
	require.True(ok)
	require.NotEmpty(warnings)
	first, ok := warnings[0].(map[string]any)
	require.True(ok)
	assert.Equal("unresolved_placeholder", first["code"])
	assert.Equal("error", first["severity"])
	found := false
	for _, w := range warnings {
		warning, ok := w.(map[string]any)
		require.True(ok)
		found = found || warning["code"] == "recipient_provenance"
	}
	assert.True(found)
}

func TestValidateOnlyWritesNothing(t *testing.T) {
	assert := assert.New(t)

	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		assert.Fail("validation contacted provider")
		return nil, errors.New("unexpected call")
	}
	events, err := f.run(t, a, "--body", "Hi FIRST_NAME", "--validate-only", "--strict", "--json")
	require.NoError(t, err)
	got := lintObject(t, events)
	assert.Equal("validated", got["status"])
	assert.Equal(false, got["valid"])
	assert.NotContains(got, "draft_id")
	assert.NotContains(got, "recipient_provenance_status")
	assertDraftRows(t, f.store)
}
func TestGmailDraftReplyValidateOnlyChecksScopes(t *testing.T) {
	f := newGmailDraftTestFixture(t)
	_, state := setupCommandOAuth(t)
	putCommandToken(t, state, f.source.Identifier, []string{oauth.ScopeGmailCompose})
	state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-once")
	f.adapter.config = state.cfg
	var events []api.CLIRunEvent
	err := f.adapter.runCLIReplyDraft(withInvocation(t.Context(), state), api.CLIRunRequest{Args: []string{
		"draft-reply", strconv.FormatInt(f.parentID, 10), "--body", "Thanks", "--validate-only", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	require.EqualError(t, err, "insufficient_scope")
	assert.Empty(t, events)
	assert.Zero(t, f.client.listCalls)
	assert.Zero(t, f.client.createCalls)
	assertDraftRows(t, f.store)
}

func TestValidateOnlyReplyAllMatchesCreate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftReplyFixture(t)
	raw, err := f.store.GetMessageRawContext(t.Context(), f.parentID)
	require.NoError(err)
	require.NoError(f.store.UpsertMessageRaw(f.parentID, bytes.Replace(raw, []byte("Subject:"), []byte("Cc: copy@example.com\r\nSubject:"), 1)))
	a := f.grantedAdapter()
	events, err := f.run(t, a, "--body", "ok", "--all", "--validate-only", "--json")
	require.NoError(err)
	recipients, ok := lintObject(t, events)["recipients"].(map[string]any)
	require.True(ok)
	events, err = f.run(t, a, "--body", "ok", "--all", "--json")
	require.NoError(err)
	var created draftReplyOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &created))
	raw, err = f.store.GetMessageRawContext(t.Context(), created.MessageID)
	require.NoError(err)
	parsed, err := msgmime.Parse(raw)
	require.NoError(err)
	assert.Equal([]any{"copy@example.com"}, recipients["cc"])
	for _, field := range []struct {
		name   string
		values []msgmime.Address
	}{{"to", parsed.To}, {"cc", parsed.Cc}, {"bcc", parsed.Bcc}} {
		values := make([]any, 0, len(field.values))
		for _, v := range field.values {
			values = append(values, v.Email)
		}
		assert.Equal(values, recipients[field.name], field.name)
	}
}

func TestDraftLintParsers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, flag := range []string{"strict", "validate-only"} {
		for _, value := range []string{"", "=true", "=false", "=invalid", "duplicate"} {
			flags := []string{"--" + flag + value}
			if value == "duplicate" {
				flags = []string{"--" + flag + "=false", "--" + flag}
			}
			args := append([]string{"draft-reply", "1", "--body", "ok"}, flags...)
			parsed, err := parseDraftReplyArgs(args)
			if value == "=invalid" || value == "duplicate" {
				require.Error(err)
				continue
			}
			require.NoError(err)
			enabled := value != "=false"
			want := draftLintOptions{}
			switch flag {
			case "strict":
				want.Strict = enabled
			case "validate-only":
				want.ValidateOnly = enabled
			}
			assert.Equal(want, parsed.draftLintOptions)
		}
	}

	got, err := parseDraftComposeArgs([]string{"draft-compose", "--source-id", "1", "--to", "a@example.com", "--body", "ok", "--strict"})
	require.NoError(err)
	assert.True(got.Strict)
	forward, err := parseDraftForwardArgs([]string{"draft-forward", "1", "--source-id", "1", "--to", "a@example.com", "--body", "ok", "--validate-only"})
	require.NoError(err)
	assert.True(forward.ValidateOnly)

	for _, command := range []string{"draft-get", "draft-delete", "draft-recover"} {
		args := []string{command, "draft"}
		if command != "draft-get" {
			args = append(args, "--revision", "1")
		}
		for _, flag := range []string{"strict", "validate-only"} {
			_, err := parseDraftLifecycleArgs(append(append([]string{}, args...), "--"+flag+"=false"))
			require.NoError(err)
			_, err = parseDraftLifecycleArgs(append(append([]string{}, args...), "--"+flag))
			require.Error(err)
		}
	}
}

func TestValidateOnlyEditWritesNothing(t *testing.T) {
	t.Run("imap", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		f := newDraftReplyFixture(t)
		a := f.grantedAdapter()
		events, err := f.run(t, a, "--body", "ok", "--json")
		require.NoError(err)
		var created draftReplyOutput
		require.NoError(json.Unmarshal([]byte(events[0].Data), &created))
		before, err := f.store.GetMessageRawContext(t.Context(), created.MessageID)
		require.NoError(err)
		a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
			assert.Fail("edit validation contacted provider")
			return nil, errors.New("unexpected call")
		}
		events, err = lintRun(t, a, nil, "draft-edit", created.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.NoError(err)
		assert.Equal(false, lintObject(t, events)["valid"])
		draft, err := f.store.GetIMAPDraftContext(t.Context(), created.DraftID)
		require.NoError(err)
		assert.Equal(int64(1), draft.Revision)
		assert.Nil(draft.Pending)
		after, err := f.store.GetMessageRawContext(t.Context(), created.MessageID)
		require.NoError(err)
		assert.Equal(before, after)
	})
	t.Run("gmail lock", func(t *testing.T) {
		f := newGmailDraftTestFixture(t)
		draft := f.seedDraft(t)
		f.client.getDraft.Message.ID = "gmail-message-external-attachment"
		f.client.getDraft.Message.Raw = gmailDraftTestRawWithAttachment("gmail-external-attachment@example.test")
		_, err := f.lifecycle(t, api.CLIRunDraftEditCommand, draft, "candidate")
		require.EqualError(t, err, "changed_externally")
		adopted, err := f.store.GetGmailDraftContext(t.Context(), draft.DraftID)
		require.NoError(t, err)
		ownership, err := f.store.AcquireSyncExecutionContext(t.Context(), f.source.ID)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ownership.Release() })
		events, err := lintRun(t, f.adapter, nil, "draft-edit", adopted.DraftID, "--revision", strconv.FormatInt(adopted.Revision, 10), "--body", "candidate", "--validate-only", "--json")
		require.EqualError(t, err, "invalid_draft")
		assert.Empty(t, events)
	})

}
func TestGmailDraftEditValidateOnlyChecksScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newGmailDraftTestFixture(t)
	draft := f.seedDraft(t)
	_, state := setupCommandOAuth(t)
	putCommandToken(t, state, f.source.Identifier, oauth.ScopesGmailReadonly)
	f.adapter.config = state.cfg
	var scopeEvents []api.CLIRunEvent
	err := f.adapter.runCLIDraftLifecycle(withInvocation(t.Context(), state), api.CLIRunRequest{Args: []string{
		"draft-edit", draft.DraftID, "--revision", "1", "--body", "Thanks", "--validate-only", "--json",
	}}, func(event api.CLIRunEvent) error {
		scopeEvents = append(scopeEvents, event)
		return nil
	})
	require.EqualError(err, "insufficient_scope")
	assert.Empty(scopeEvents)
	assert.Zero(f.client.getCalls)
	assert.Zero(f.client.updateCalls)
}

func TestDraftChatPreviewAndLint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic")
	require.NoError(err)
	id, err := st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
	require.NoError(err)
	a := &storeAPIAdapter{store: st}
	for _, scoped := range []bool{true, false} {
		grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}}
		want := "invalid_destination"
		if !scoped {
			grant.Sources[0].Identifier = "other"
			want = "not_permitted"
		}
		_, err := lintRun(t, a, grant, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--reply-to", "99999", "--body", "Thanks", "--validate-only")
		require.EqualError(err, want)
	}
	events, err := lintRun(t, a, nil, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--body", "Hi Sam,", "--validate-only", "--json")
	require.NoError(err)
	assert.Nil(lintObject(t, events)["recipients"])
	_, err = lintRun(t, a, nil, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--body", "Hi FIRST_NAME", "--strict", "--json")
	require.EqualError(err, "draft_lint_failed")
	assertDraftRows(t, st)
	events, err = lintRun(t, a, nil, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--body", "FIRST_NAME", "--json")
	require.NoError(err)
	created := lintObject(t, events)
	assert.Contains(events[0].Data, "unresolved_placeholder")
	draftID, ok := created["draft_id"].(string)
	require.True(ok)
	draft, err := st.GetChatDraftContext(t.Context(), draftID)
	require.NoError(err)
	_, err = lintRun(t, a, nil, "draft-edit", draftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
	require.NoError(err)
	after, err := st.GetChatDraftContext(t.Context(), draftID)
	require.NoError(err)
	assert.Equal(draft, after)
	_, err = lintRun(t, a, nil, "draft-edit", draftID, "--revision", "1", "--body", "LAST_NAME", "--strict", "--json")
	require.EqualError(err, "draft_lint_failed")
	after, err = st.GetChatDraftContext(t.Context(), draftID)
	require.NoError(err)
	assert.Equal(int64(1), after.Revision)
	events, err = lintRun(t, a, nil, "draft-edit", draftID, "--revision", "1", "--body", "LAST_NAME", "--json")
	require.NoError(err)
	assert.Contains(events[0].Data, "unresolved_placeholder")
}

func TestDraftBeeperPreviewAndLint(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		f := newBeeperDraftFixture(t)
		for _, edit := range []bool{false, true} {
			t.Run(fmt.Sprintf("edit=%t", edit), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				args := []string{"draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat}
				if edit {
					draft, err := f.create(t, nil, "existing")
					require.NoError(err)
					args = []string{"draft-edit", draft.DraftID, "--revision", strconv.FormatInt(draft.Revision, 10)}
				}
				beforeRequests, beforePatches := f.beeper.counts()
				events, err := lintRun(t, f.adapter, nil, append(args, "--body", "  ", "--validate-only", "--json")...)
				require.NoError(err)
				got := lintObject(t, events)
				assert.Equal(false, got["valid"])
				warnings, ok := got["warnings"].([]any)
				require.True(ok)
				require.Len(warnings, 1)
				warning, ok := warnings[0].(map[string]any)
				require.True(ok)
				assert.Equal("empty_body", warning["code"])
				_, err = lintRun(t, f.adapter, nil, append(args, "--body", "\x00", "--validate-only", "--json")...)
				require.EqualError(err, "invalid_args")
				_, err = lintRun(t, f.adapter, nil, append(args, "--body", "  ")...)
				require.EqualError(err, "invalid_args")
				requests, patches := f.beeper.counts()
				assert.Equal(beforeRequests, requests)
				assert.Equal(beforePatches, patches)
			})
		}
	})
	t.Run("archived", func(t *testing.T) {
		f := newBeeperDraftFixture(t)
		for _, otherAccount := range []bool{false, true} {
			t.Run(fmt.Sprintf("other_account=%t", otherAccount), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				chatID := "!missing:beeper.local"
				if otherAccount {
					other, err := f.store.GetOrCreateSource("beeper", "other-account")
					require.NoError(err)
					_, err = f.store.EnsureConversationWithType(other.ID, chatID, "channel", "Other chat")
					require.NoError(err)
				}
				_, err := lintRun(t, f.adapter, nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", chatID, "--body", "Thanks", "--validate-only", "--json")
				require.EqualError(err, "chat_not_found")
				require.ErrorContains(errors.Unwrap(err), "not archived under the selected source")
				requests, patches := f.beeper.counts()
				assert.Zero(requests)
				assert.Zero(patches)
				assertDraftRows(t, f.store)
			})
		}
	})
	t.Run("pending and duplicate", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		f := newBeeperDraftFixture(t)
		draft, err := f.store.CreateBeeperDraftContext(t.Context(), f.source.ID, beeperDraftTestChat, "original")
		require.NoError(err)
		grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: f.source.ID, Type: f.source.SourceType, Identifier: f.source.Identifier}}}
		events, err := lintRun(t, f.adapter, grant, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "  "+beeperDraftTestChat+"  ", "--body", "Thanks", "--validate-only", "--json")
		require.EqualError(err, "draft_exists")
		assert.Equal(draft.DraftID, lintObject(t, events)["draft_id"])
		_, err = lintRun(t, f.adapter, nil, "draft-edit", draft.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.EqualError(err, "pending_operation")
		pending, err := f.store.GetBeeperDraftContext(t.Context(), draft.DraftID)
		require.NoError(err)
		assert.Equal(draft, pending)
		draft, err = f.store.FinishBeeperDraftContext(t.Context(), draft.DraftID, draft.Revision, "original")
		require.NoError(err)
		_, err = lintRun(t, f.adapter, nil, "draft-edit", draft.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.NoError(err)
		requests, patches := f.beeper.counts()
		assert.Zero(requests)
		assert.Zero(patches)
		after, err := f.store.GetBeeperDraftContext(t.Context(), draft.DraftID)
		require.NoError(err)
		assert.Equal(draft, after)
	})
	t.Run("lint", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		f := newBeeperDraftFixture(t)
		_, err := lintRun(t, f.adapter, nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat, "--body", "Hi FIRST_NAME", "--strict", "--json")
		require.EqualError(err, "draft_lint_failed")
		requests, patches := f.beeper.counts()
		assert.Zero(requests)
		assert.Zero(patches)
		assertDraftRows(t, f.store)
		created, err := f.run(t, nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat, "--body", "FIRST_NAME")
		require.NoError(err)
		require.NotEmpty(created.Warnings)
		assert.Equal("unresolved_placeholder", created.Warnings[0].Code)
		_, err = f.run(t, nil, "draft-edit", created.DraftID, "--revision", "1", "--body", "LAST_NAME", "--strict")
		require.EqualError(err, "draft_lint_failed")
		after, err := f.store.GetBeeperDraftContext(t.Context(), created.DraftID)
		require.NoError(err)
		assert.Equal(int64(1), after.Revision)
	})
}
