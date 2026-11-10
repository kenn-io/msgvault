package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
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
	emit := func(e api.CLIRunEvent) error { events = append(events, e); return nil }
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
func assertDraftRows(t *testing.T, st *store.Store, want int) {
	t.Helper()
	for _, table := range []string{"imap_drafts", "gmail_drafts", "chat_drafts", "beeper_drafts", "attachments"} {
		var n int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n))
		assert.Equal(t, want, n, table)
	}
}
func TestDraftReplyStrictRefuses(t *testing.T) {
	for _, provider := range []string{"imap", "gmail"} {
		t.Run(provider, func(t *testing.T) {
			var a *storeAPIAdapter
			var st *store.Store
			var id int64
			from := "owner@example.test"
			calls := 0
			providerCreates := func() int { return calls }
			if provider == "imap" {
				f := newDraftReplyFixture(t)
				a, st, id, from = f.grantedAdapter(), f.store, f.parentID, testutil.IMAPTestUsername
				a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
					calls++
					return nil, fmt.Errorf("unexpected provider call")
				}
			} else {
				f := newGmailDraftTestFixture(t)
				a, st, id, from = f.adapter, f.store, f.parentID, f.source.Identifier
				providerCreates = func() int { return f.client.createCalls }
			}
			events, err := lintRun(t, a, nil, "draft-reply", strconv.FormatInt(id, 10), "--from", from, "--body", "Hi FIRST_NAME", "--strict", "--json")
			require.EqualError(t, err, "draft_lint_failed")
			got := lintObject(t, events)
			assert.Equal(t, "refused", got["status"])
			assert.Equal(t, cliStreamStderr, events[0].Type)
			if provider == "imap" {
				events, err = lintRun(t, a, nil, "draft-reply", strconv.FormatInt(id, 10), "--from", from, "--body", "Hi FIRST_NAME", "--strict")
				require.EqualError(t, err, "draft_lint_failed")
				require.NotEmpty(t, events)
				assert.Contains(t, events[0].Data, "Refused.")
				assert.Contains(t, events[0].Data, "error: The text contains a template placeholder.")
				assert.Contains(t, events[0].Data, "Greeting check skipped.")
				for _, code := range []string{"draft_lint_failed", "unresolved_placeholder", "greeting_name_mismatch"} {
					assert.NotContains(t, events[0].Data, code)
				}
				assert.NotContains(t, events[0].Data, `{"`)
			}
			assert.Zero(t, providerCreates())
			assertDraftRows(t, st, 0)
		})
	}
}
func TestDraftReplyPlaceholderWarning(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%t", allow), func(t *testing.T) {
			f := newDraftReplyFixture(t)
			args := []string{"--body", "Hi FIRST_NAME", "--json"}
			if allow {
				args = append(args, "--strict", "--allow-placeholders")
			}
			events, err := f.run(t, f.grantedAdapter(), args...)
			require.NoError(t, err)
			got := lintObject(t, events)
			warnings := got["warnings"].([]any)
			assert.Equal(t, "unresolved_placeholder", warnings[0].(map[string]any)["code"])
			severity := "error"
			if allow {
				severity = "warn"
			}
			assert.Equal(t, severity, warnings[0].(map[string]any)["severity"])
			found := false
			for _, w := range warnings {
				if w.(map[string]any)["code"] == "recipient_provenance" {
					found = true
					assert.Equal(t, "warn", w.(map[string]any)["severity"])
					assert.Equal(t, "to", w.(map[string]any)["field"])
				}
			}
			assert.True(t, found)
		})
	}
}

func TestDraftReplyDelegatedSkipsNamesWithoutRead(t *testing.T) {
	f := newDraftReplyFixture(t)
	id, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	require.NoError(t, err)
	person, _, err := f.store.CreatePersonFromParticipantContext(t.Context(), id)
	require.NoError(t, err)
	given := "Pat"
	_, err = f.store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameStructured, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, GivenName: &given})
	require.NoError(t, err)
	for _, read := range []bool{false, true} {
		t.Run(fmt.Sprintf("read=%t", read), func(t *testing.T) {
			grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: f.source.ID, Type: f.source.SourceType, Identifier: f.source.Identifier, SenderKeys: []string{store.NormalizeIdentifierForCompare(testutil.IMAPTestUsername)}}}}
			if read {
				grant.Permissions = append(grant.Permissions, agentgrant.PermissionMessageRead)
			}
			events, err := f.runAs(t, f.grantedAdapter(), grant, "--body", "Hi Alex,", "--json")
			require.NoError(t, err)
			var got draftReplyOutput
			require.NoError(t, json.Unmarshal([]byte(events[0].Data), &got))
			if read {
				require.Len(t, got.Warnings, 1)
				assert.Equal(t, "greeting_name_mismatch", got.Warnings[0].Code)
				assert.Empty(t, got.LintSkipped)
			} else {
				assert.Empty(t, got.Warnings)
				assert.Equal(t, []string{"greeting_name_mismatch"}, got.LintSkipped)
			}
		})
	}
}

func TestValidateOnlyWritesNothing(t *testing.T) {
	t.Run("imap", func(t *testing.T) {
		f := newDraftReplyFixture(t)
		a := f.grantedAdapter()
		a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
			assert.Fail(t, "validation contacted provider")
			return nil, fmt.Errorf("unexpected call")
		}
		events, err := f.run(t, a, "--body", "Hi FIRST_NAME", "--validate-only", "--strict", "--json")
		require.NoError(t, err)
		got := lintObject(t, events)
		assert.Equal(t, "validated", got["status"])
		assert.Equal(t, false, got["valid"])
		assert.NotContains(t, got, "draft_id")
		assert.NotContains(t, got, "recipient_provenance_status")
		assertDraftRows(t, f.store, 0)
	})
	t.Run("gmail", func(t *testing.T) {
		f := newGmailDraftTestFixture(t)
		events, err := lintRun(t, f.adapter, nil, "draft-reply", strconv.FormatInt(f.parentID, 10), "--body", "ok", "--validate-only", "--json")
		require.NoError(t, err)
		assert.Equal(t, true, lintObject(t, events)["valid"])
		assert.Zero(t, f.client.createCalls)
		assert.Zero(t, f.client.listCalls)
		assertDraftRows(t, f.store, 0)
	})
	t.Run("chat", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("slack", "synthetic")
		require.NoError(t, err)
		id, err := st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
		require.NoError(t, err)
		a := &storeAPIAdapter{store: st}
		events, err := lintRun(t, a, nil, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--body", "Hi Sam,", "--validate-only", "--json")
		require.NoError(t, err)
		got := lintObject(t, events)
		assert.Nil(t, got["recipients"])
		assert.Equal(t, []any{"greeting_name_mismatch"}, got["lint_skipped"])
		assertDraftRows(t, st, 0)
	})
	t.Run("beeper", func(t *testing.T) {
		f := newBeeperDraftFixture(t)
		_, err := lintRun(t, f.adapter, nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat, "--body", "Hi Sam,", "--validate-only", "--json")
		require.NoError(t, err)
		requests, patches := f.beeper.counts()
		assert.Zero(t, requests)
		assert.Zero(t, patches)
		assertDraftRows(t, f.store, 0)
	})
}
func TestValidateOnlyReplyAllMatchesCreate(t *testing.T) {
	f := newDraftReplyFixture(t)
	raw, err := f.store.GetMessageRawContext(t.Context(), f.parentID)
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertMessageRaw(f.parentID, bytes.Replace(raw, []byte("Subject:"), []byte("Cc: copy@example.com\r\nSubject:"), 1)))
	a := f.grantedAdapter()
	events, err := f.run(t, a, "--body", "ok", "--all", "--validate-only", "--json")
	require.NoError(t, err)
	recipients := lintObject(t, events)["recipients"].(map[string]any)
	events, err = f.run(t, a, "--body", "ok", "--all", "--json")
	require.NoError(t, err)
	var created draftReplyOutput
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &created))
	raw, err = f.store.GetMessageRawContext(t.Context(), created.MessageID)
	require.NoError(t, err)
	parsed, err := imaplib.BuildIMAPDraftReplacement(raw, "ok", time.Now(), "")
	require.NoError(t, err)
	assert.Equal(t, []any{"copy@example.com"}, recipients["cc"])
	for _, field := range []struct {
		name   string
		values []msgmime.Address
	}{{"to", parsed.Parsed.To}, {"cc", parsed.Parsed.Cc}, {"bcc", parsed.Parsed.Bcc}} {
		values := make([]any, 0, len(field.values))
		for _, v := range field.values {
			values = append(values, v.Email)
		}
		assert.Equal(t, values, recipients[field.name], field.name)
	}
}

func TestDraftLintParsers(t *testing.T) {
	// Exercise each existing command parser, including forward delegation.
	for _, tc := range []struct {
		name  string
		parse func([]string) (draftLintOptions, error)
		args  []string
	}{
		{"reply", func(args []string) (draftLintOptions, error) {
			v, e := parseDraftReplyArgs(args)
			return v.draftLintOptions, e
		}, []string{"draft-reply", "1", "--body", "ok"}},
		{"compose", func(args []string) (draftLintOptions, error) {
			v, e := parseDraftComposeArgs(args)
			return v.draftLintOptions, e
		}, []string{"draft-compose", "--source-id", "1", "--to", "a@example.com", "--body", "ok"}},
		{"forward", func(args []string) (draftLintOptions, error) {
			v, e := parseDraftForwardArgs(args)
			return v.draftLintOptions, e
		}, []string{"draft-forward", "1", "--source-id", "1", "--to", "a@example.com", "--body", "ok"}},
		{"edit", func(args []string) (draftLintOptions, error) {
			v, e := parseDraftLifecycleArgs(args)
			return v.draftLintOptions, e
		}, []string{"draft-edit", "draft", "--revision", "1", "--body", "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, flag := range []string{"strict", "allow-placeholders", "validate-only"} {
				values := []string{""}
				if tc.name == "reply" {
					values = []string{"", "=true", "=false", "=invalid", "duplicate"}
				}
				for _, value := range values {
					flags := []string{"--" + flag + value}
					if value == "duplicate" {
						flags = []string{"--" + flag + "=false", "--" + flag}
					}
					opts, err := tc.parse(append(append([]string{}, tc.args...), flags...))
					if value == "=invalid" || value == "duplicate" {
						require.Error(t, err)
						continue
					}
					require.NoError(t, err)
					enabled := value != "=false"
					want := draftLintOptions{}
					switch flag {
					case "strict":
						want.Strict = enabled
					case "allow-placeholders":
						want.AllowPlaceholders = enabled
					case "validate-only":
						want.ValidateOnly = enabled
					}
					assert.Equal(t, want, opts)
				}
			}
		})
	}
	got, err := parseDraftComposeArgs([]string{"draft-compose", "--source-id", "1", "--to", "a@example.com", "--body", "--strict"})
	require.NoError(t, err)
	assert.Equal(t, "--strict", got.Body)
	assert.Equal(t, draftLintOptions{}, got.draftLintOptions)

	for _, command := range []string{"draft-get", "draft-delete", "draft-recover"} {
		args := []string{command, "draft"}
		if command != "draft-get" {
			args = append(args, "--revision", "1")
		}
		for _, flag := range []string{"strict", "allow-placeholders", "validate-only"} {
			_, err := parseDraftLifecycleArgs(append(append([]string{}, args...), "--"+flag+"=false"))
			require.NoError(t, err)
			_, err = parseDraftLifecycleArgs(append(append([]string{}, args...), "--"+flag))
			require.Error(t, err)
		}
	}
}

func TestValidateOnlyEditWritesNothing(t *testing.T) {
	t.Run("imap", func(t *testing.T) {
		f := newDraftReplyFixture(t)
		a := f.grantedAdapter()
		events, err := f.run(t, a, "--body", "ok", "--json")
		require.NoError(t, err)
		var created draftReplyOutput
		require.NoError(t, json.Unmarshal([]byte(events[0].Data), &created))
		before, err := f.store.GetMessageRawContext(t.Context(), created.MessageID)
		require.NoError(t, err)
		a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
			assert.Fail(t, "edit validation contacted provider")
			return nil, fmt.Errorf("unexpected call")
		}
		events, err = lintRun(t, a, nil, "draft-edit", created.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.NoError(t, err)
		assert.Equal(t, false, lintObject(t, events)["valid"])
		draft, err := f.store.GetIMAPDraftContext(t.Context(), created.DraftID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), draft.Revision)
		assert.Nil(t, draft.Pending)
		after, err := f.store.GetMessageRawContext(t.Context(), created.MessageID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})
	t.Run("gmail", func(t *testing.T) {
		f := newGmailDraftTestFixture(t)
		draft := f.seedDraft(t)
		before := f.client.getCalls
		events, err := lintRun(t, f.adapter, nil, "draft-edit", draft.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.NoError(t, err)
		assert.Equal(t, false, lintObject(t, events)["valid"])
		assert.Equal(t, before, f.client.getCalls)
		assert.Zero(t, f.client.updateCalls)
		after, err := f.store.GetGmailDraftContext(t.Context(), draft.DraftID)
		require.NoError(t, err)
		assert.Equal(t, draft.Revision, after.Revision)
		assert.Nil(t, after.Pending)
	})
	t.Run("chat", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		s, err := st.GetOrCreateSource("slack", "synthetic")
		require.NoError(t, err)
		id, err := st.EnsureConversationWithType(s.ID, "C1", "channel", "General")
		require.NoError(t, err)
		draft, err := st.CreateChatDraftContext(t.Context(), id, 0, "original", func(string, string) error { return nil })
		require.NoError(t, err)
		a := &storeAPIAdapter{store: st}
		_, err = lintRun(t, a, nil, "draft-edit", draft.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.NoError(t, err)
		after, err := st.GetChatDraftContext(t.Context(), draft.DraftID)
		require.NoError(t, err)
		assert.Equal(t, draft, after)
	})
	t.Run("beeper", func(t *testing.T) {
		f := newBeeperDraftFixture(t)
		draft, err := f.store.CreateBeeperDraftContext(t.Context(), f.source.ID, beeperDraftTestChat, "original")
		require.NoError(t, err)
		_, err = lintRun(t, f.adapter, nil, "draft-edit", draft.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.EqualError(t, err, "pending_operation")
		pending, err := f.store.GetBeeperDraftContext(t.Context(), draft.DraftID)
		require.NoError(t, err)
		assert.Equal(t, draft, pending)
		draft, err = f.store.FinishBeeperDraftContext(t.Context(), draft.DraftID, draft.Revision, "original")
		require.NoError(t, err)
		_, err = lintRun(t, f.adapter, nil, "draft-edit", draft.DraftID, "--revision", "1", "--body", "Hi FIRST_NAME", "--validate-only", "--json")
		require.NoError(t, err)
		requests, patches := f.beeper.counts()
		assert.Zero(t, requests)
		assert.Zero(t, patches)
		after, err := f.store.GetBeeperDraftContext(t.Context(), draft.DraftID)
		require.NoError(t, err)
		assert.Equal(t, draft, after)
	})
}
func TestGmailDraftEditValidateOnlyChecksScope(t *testing.T) {
	f := newGmailDraftTestFixture(t)
	draft := f.seedDraft(t)
	before := f.client.getCalls
	var events []api.CLIRunEvent

	// Saved read-only scopes must refuse preview before provider access.
	_, state := setupCommandOAuth(t)
	putCommandToken(t, state, f.source.Identifier, oauth.ScopesGmailReadonly)
	f.adapter.config = state.cfg
	err := f.adapter.runCLIDraftLifecycle(withInvocation(t.Context(), state), api.CLIRunRequest{Args: []string{
		"draft-edit", draft.DraftID, "--revision", "1", "--body", "Thanks", "--validate-only", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	require.EqualError(t, err, "insufficient_scope")
	assert.Empty(t, events)
	assert.Equal(t, before, f.client.getCalls)
	assert.Zero(t, f.client.updateCalls)
}

func TestDraftForwardValidateAndStrict(t *testing.T) {
	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	calls := 0
	a.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		calls++
		return nil, fmt.Errorf("unexpected provider call")
	}
	args := []string{"draft-forward", strconv.FormatInt(f.parentID, 10), "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "a@example.com", "--body", "FIRST_NAME", "--json"}
	events, err := lintRun(t, a, nil, append(args, "--validate-only")...)
	require.NoError(t, err)
	assert.Equal(t, false, lintObject(t, events)["valid"])
	_, err = lintRun(t, a, nil, append(args, "--strict")...)
	require.EqualError(t, err, "draft_lint_failed")
	assertDraftRows(t, f.store, 0)
	assert.Zero(t, calls)
}

func TestDraftChatAndBeeperReportLint(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		s, err := st.GetOrCreateSource("slack", "synthetic")
		require.NoError(t, err)
		id, err := st.EnsureConversationWithType(s.ID, "C1", "channel", "General")
		require.NoError(t, err)
		a := &storeAPIAdapter{store: st}
		_, err = lintRun(t, a, nil, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--body", "Hi FIRST_NAME", "--strict", "--json")
		require.EqualError(t, err, "draft_lint_failed")
		assertDraftRows(t, st, 0)
		events, err := lintRun(t, a, nil, "draft-compose", "--conversation", strconv.FormatInt(id, 10), "--body", "FIRST_NAME", "--json")
		require.NoError(t, err)
		created := lintObject(t, events)
		assert.Contains(t, events[0].Data, "unresolved_placeholder")
		draftID := created["draft_id"].(string)
		_, err = lintRun(t, a, nil, "draft-edit", draftID, "--revision", "1", "--body", "LAST_NAME", "--strict", "--json")
		require.EqualError(t, err, "draft_lint_failed")
		after, err := st.GetChatDraftContext(t.Context(), draftID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), after.Revision)
		events, err = lintRun(t, a, nil, "draft-edit", draftID, "--revision", "1", "--body", "LAST_NAME", "--json")
		require.NoError(t, err)
		assert.Contains(t, events[0].Data, "unresolved_placeholder")
	})
	t.Run("beeper", func(t *testing.T) {
		f := newBeeperDraftFixture(t)
		_, err := lintRun(t, f.adapter, nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat, "--body", "Hi FIRST_NAME", "--strict", "--json")
		require.EqualError(t, err, "draft_lint_failed")
		requests, patches := f.beeper.counts()
		assert.Zero(t, requests)
		assert.Zero(t, patches)
		assertDraftRows(t, f.store, 0)
		created, err := f.run(t, nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat, "--body", "FIRST_NAME")
		require.NoError(t, err)
		require.NotEmpty(t, created.Warnings)
		assert.Equal(t, "unresolved_placeholder", created.Warnings[0].Code)
		_, err = f.run(t, nil, "draft-edit", created.DraftID, "--revision", "1", "--body", "LAST_NAME", "--strict")
		require.EqualError(t, err, "draft_lint_failed")
		after, err := f.store.GetBeeperDraftContext(t.Context(), created.DraftID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), after.Revision)
	})
}
