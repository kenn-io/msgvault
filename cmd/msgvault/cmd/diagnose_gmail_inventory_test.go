package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func inventoryMessage(t *testing.T, st *store.Store, sourceID int64, id string) int64 {
	t.Helper()
	conversation, err := st.EnsureConversation(t.Context(), sourceID, "synthetic-thread", "Synthetic inventory")
	require.NoError(t, err)
	message, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conversation, SourceMessageID: id, MessageType: store.MessageTypeEmail})
	require.NoError(t, err)
	return message
}

// inventoryProvider replaces only the external HTTP boundary. Pagination,
// query scope, decoding, deadlines and the Gmail client are production code.
// Tests using this helper remain serial because DefaultTransport is global.
func inventoryProvider(t *testing.T, reply func(*http.Request) (string, error)) func(context.Context, *store.Source) (gmail.API, error) {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method, "observation must never send a provider write")
		require.Equal(t, "gmail.googleapis.com", req.URL.Host)
		body, err := reply(req)
		if err != nil {
			if req.Context().Err() != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":400,"message":"synthetic provider unavailable"}}`)), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	return func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-inventory-token"}), gmail.WithRateLimiter(gmail.NewRateLimiter(100000))), nil
	}
}

func inventoryPage(ids []string, next string) string {
	messages := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		messages = append(messages, map[string]string{"id": id})
	}
	// Use the provider's wire spelling, not the client's Go response fields.
	data, _ := json.Marshal(map[string]any{"messages": messages, "nextPageToken": next, "resultSizeEstimate": 3})
	return string(data)
}

func inventoryProfile(history string) string {
	return `{"emailAddress":"inventory@example.test","historyId":"` + history + `"}`
}

func inventoryOptions(id int64) gmailInventoryOptions {
	return gmailInventoryOptions{SourceID: id, Limit: 100, MaxMessages: 1000, MaxPages: 100, Timeout: time.Minute}
}

// serial: production Gmail clients use the synthetic DefaultTransport.
func TestGmailInventorySets(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		local, remote, missing, extra []string
		shared                        int
	}{
		{name: "identical", local: []string{"a", "b"}, remote: []string{"b", "a"}, shared: 2},
		{name: "remote_only", local: []string{"a"}, remote: []string{"a", "b"}, missing: []string{"b"}, shared: 1},
		{name: "local_only", local: []string{"a", "d"}, remote: []string{"a"}, extra: []string{"d"}, shared: 1},
		{name: "equal_counts_different_sets", local: []string{"a", "b", "d"}, remote: []string{"a", "b", "c"}, missing: []string{"c"}, extra: []string{"d"}, shared: 2},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
			require.NoError(err)
			other, err := st.GetOrCreateSource(sourceTypeGmail, "other@example.test")
			require.NoError(err)
			for _, id := range tc.local {
				inventoryMessage(t, st, source.ID, id)
			}
			inventoryMessage(t, st, other.ID, "c") // Must not hide the missing identity in selected source.
			var paths []string
			factory := inventoryProvider(t, func(req *http.Request) (string, error) {
				paths = append(paths, req.URL.Path)
				switch req.URL.Path {
				case "/gmail/v1/users/me/profile":
					return inventoryProfile("100"), nil
				case "/gmail/v1/users/me/messages":
					assert.Equal("true", req.URL.Query().Get("includeSpamTrash"))
					assert.Equal("500", req.URL.Query().Get("maxResults"))
					assert.False(req.URL.Query().Has("q"))
					assert.False(req.URL.Query().Has("labelIds"))
					return inventoryPage(tc.remote, ""), nil
				default:
					return "", fmt.Errorf("forbidden inventory request %s", req.URL.Path)
				}
			})

			runID, runErr := st.StartSyncContext(t.Context(), source.ID, "full")
			require.NoError(runErr)
			require.NoError(st.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: runID, SourceMessageID: "pending", Phase: "fetch", Status: store.SyncRunItemStatusError, ErrorKind: "fetch", ErrorMessage: "synthetic acquisition failure"}))
			if len(tc.local) > 0 {
				ids, lookupErr := st.MessageExistsBatchContext(t.Context(), source.ID, tc.local)
				require.NoError(lookupErr)
				require.NoError(st.UpsertMessageRaw(ids[tc.local[0]], []byte("From: sender@example.test\r\n\r\nSynthetic body")))
			}
			before := inventoryArchiveState(t, st)
			result, err := collectGmailInventory(t.Context(), st, factory, inventoryOptions(source.ID))
			require.NoError(err)
			assert.Equal("complete", result.Status)
			assert.True(result.RemoteComplete)
			assert.True(result.LocalComplete)
			require.NotNil(result.RemoteOnly)
			require.NotNil(result.LocalNotObserved)
			assert.ElementsMatch(tc.missing, result.RemoteOnly.IDs)
			assert.ElementsMatch(tc.extra, result.LocalNotObserved.IDs)
			assert.Equal(len(tc.local), result.LocalCount)
			assert.Equal(len(tc.remote), result.RemoteCount)
			assert.Equal(tc.shared, result.SharedCount)
			assert.Equal([]string{"/gmail/v1/users/me/profile", "/gmail/v1/users/me/messages", "/gmail/v1/users/me/profile"}, paths)
			assert.Equal(before, inventoryArchiveState(t, st), "diagnosis must not mutate archived rows or ledger")
		})
	}
}

// serial: production Gmail clients use the synthetic DefaultTransport.
func TestGmailInventoryPagination(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		mode                         string
		maxPages, maxMessages, limit int
		status, reason               string
		pages                        int
	}{
		{"complete_with_empty_middle_and_duplicates", "complete", 10, 10, 1, "complete", "pagination_finished", 3},
		{"page_limit", "complete", 1, 10, 1, "partial", "page_limit", 1},
		{"message_limit", "complete", 10, 1, 1, "partial", "message_limit", 3},
		{"first_page_failure", "first_error", 10, 10, 1, "partial", "page_failed", 0},
		{"later_page_failure", "error", 10, 10, 1, "partial", "page_failed", 1},
		{"malformed_json", "decode", 10, 10, 1, "partial", "page_failed", 1},
		{"empty_id", "empty_id", 10, 10, 1, "partial", "invalid_message_id", 2},
		{"repeated_token_cycle", "cycle", 10, 10, 1, "partial", "repeated_page_token", 3},
		{"mailbox_changed", "churn", 10, 10, 1, "inconclusive", "mailbox_changed_or_history_unknown", 3},
		{"summary_only", "complete", 10, 10, 0, "complete", "pagination_finished", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
			require.NoError(err)
			inventoryMessage(t, st, source.ID, "local-only")
			profiles := 0
			var tokens []string
			factory := inventoryProvider(t, func(req *http.Request) (string, error) {
				if strings.HasSuffix(req.URL.Path, "/profile") {
					profiles++
					if tc.mode == "churn" && profiles == 2 {
						return inventoryProfile("101"), nil
					}
					return inventoryProfile("100"), nil
				}
				require.True(strings.HasSuffix(req.URL.Path, "/messages"))
				token := req.URL.Query().Get("pageToken")
				tokens = append(tokens, token)
				if tc.mode == "first_error" || tc.mode == "error" && token == "second" {
					return "", errors.New("synthetic provider unavailable")
				}
				if tc.mode == "decode" && token == "second" {
					return "{", nil
				}
				if tc.mode == "empty_id" && token == "second" {
					return inventoryPage([]string{""}, ""), nil
				}
				switch token {
				case "":
					return inventoryPage([]string{"b"}, "second"), nil
				case "second":
					return inventoryPage(nil, "third"), nil
				case "third":
					if tc.mode == "cycle" {
						return inventoryPage(nil, "second"), nil
					}
					return inventoryPage([]string{"a", "b"}, ""), nil
				default:
					return "", errors.New("unexpected token")
				}
			})
			opts := inventoryOptions(source.ID)
			opts.MaxPages = tc.maxPages
			opts.MaxMessages = tc.maxMessages
			opts.Limit = tc.limit
			result, err := collectGmailInventory(t.Context(), st, factory, opts)
			if tc.status == "complete" {
				require.NoError(err)
			} else {
				require.Error(err)
			}
			assert.Equal(tc.status, result.Status)
			assert.Equal(tc.reason, result.Reason)
			assert.Equal(tc.pages, result.Pages)
			if tc.status == "complete" {
				assert.True(result.RemoteComplete)
				assert.Equal(2, result.RemoteCount)
				assert.Equal(1, result.DuplicateObservations)
				assert.Equal([]string{"", "second", "third"}, tokens)
				assert.Equal(2, result.RemoteOnly.Count)
				assert.Equal(2-tc.limit, result.RemoteOnly.Omitted)
				if tc.limit == 1 {
					assert.Equal([]string{"a"}, result.RemoteOnly.IDs)
				}
				require.NotNil(result.LocalNotObserved)
				assert.Equal(1, result.LocalNotObserved.Count)
			} else {
				assert.Nil(result.LocalNotObserved, "incomplete or changed mailbox cannot establish remote absence")
				assert.Equal(1, result.LocalUnassessed)
			}
			var out bytes.Buffer
			require.NoError(writeGmailInventory(&out, result, true))
			var decoded gmailInventoryResult
			require.NoError(json.Unmarshal(out.Bytes(), &decoded))
			assert.Equal(result, decoded)
			assert.NotContains(out.String(), "synthetic provider unavailable")
		})
	}
}

// serial: production Gmail clients use the synthetic DefaultTransport.
func TestGmailInventoryCancellation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
	require.NoError(err)
	inventoryMessage(t, st, source.ID, "unseen-local")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	factory := inventoryProvider(t, func(req *http.Request) (string, error) {
		if strings.HasSuffix(req.URL.Path, "/profile") {
			return inventoryProfile("100"), nil
		}
		if req.URL.Query().Get("pageToken") == "" {
			return inventoryPage([]string{"observed"}, "second"), nil
		}
		// Cancellation is triggered by reaching the second request, independent of
		// how long the runner took to get here.
		cancel()
		<-req.Context().Done()
		return "", req.Context().Err()
	})
	result, err := collectGmailInventory(ctx, st, factory, inventoryOptions(source.ID))
	require.ErrorIs(err, context.Canceled)
	assert.Equal("partial", result.Status)
	assert.False(result.RemoteComplete)
	assert.Equal(1, result.RemoteCount)
	assert.Nil(result.LocalNotObserved)
	assert.Equal(1, result.LocalUnassessed)
}

// serial: production Gmail clients use the synthetic DefaultTransport.
func TestGmailInventoryScopeAndConcurrentAcquisition(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
	require.NoError(err)
	hidden := inventoryMessage(t, st, source.ID, "hidden")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), hidden)
	require.NoError(err)
	historical := inventoryMessage(t, st, source.ID, "retained")
	require.NoError(st.MarkMessagesDeletedBatch(t.Context(), source.ID, []string{"retained"}))
	require.Positive(historical)
	archive := inventoryMessage(t, st, source.ID, "msgvault-archive:42:old")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO source_merge_archive_only_messages(message_id, original_source_message_id) VALUES (?,?)`), archive, "old")
	require.NoError(err)
	// The diagnostic has finished its local snapshot before provider I/O. A real
	// concurrent write must not retroactively change that copied local set.
	factory := inventoryProvider(t, func(req *http.Request) (string, error) {
		if strings.HasSuffix(req.URL.Path, "/profile") {
			return inventoryProfile("100"), nil
		}
		inventoryMessage(t, st, source.ID, "arriving")
		return inventoryPage([]string{"hidden", "spam", "trash", "arriving"}, ""), nil
	})
	result, err := collectGmailInventory(t.Context(), st, factory, inventoryOptions(source.ID))
	require.NoError(err)
	assert.Equal(2, result.LocalCount)
	assert.Equal(1, result.IncludedHidden)
	assert.Equal(1, result.ExcludedArchiveOnly)
	assert.Equal([]string{"arriving", "spam", "trash"}, result.RemoteOnly.IDs)
	assert.Equal([]string{"retained"}, result.LocalNotObserved.IDs)
	assert.Contains(result.Caveats[0], "syncing, deleting messages or changing accounts")
	var human bytes.Buffer
	require.NoError(writeGmailInventory(&human, result, false))
	assert.Contains(human.String(), "excluded historical messages from merged accounts: 1; included hidden messages: 1")
	exists, err := st.MessageExistsBatchContext(t.Context(), source.ID, []string{"arriving"})
	require.NoError(err)
	assert.Len(exists, 1)
}

func TestGmailInventoryLocalFailures(t *testing.T) {
	for _, tc := range []string{"missing_source", "not_gmail", "history_only", "merged", "local_limit", "sql_error", "invalid_local_id", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			kind := "gmail"
			if tc == "not_gmail" {
				kind = "mbox"
			}
			source, err := st.GetOrCreateSource(kind, "inventory@example.test")
			require.NoError(err)
			opts := inventoryOptions(source.ID)
			ctx := t.Context()
			switch tc {
			case "missing_source":
				opts.SourceID = 9999999
			case "history_only":
				_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO source_settings(source_id, history_only) VALUES (?, TRUE)`), source.ID)
				require.NoError(err)
			case "merged":
				into, createErr := st.GetOrCreateSource(sourceTypeGmail, "destination@example.test")
				require.NoError(createErr)
				_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: into.ID})
				require.NoError(err)
			case "local_limit":
				opts.MaxMessages = 1
				inventoryMessage(t, st, source.ID, "a")
				inventoryMessage(t, st, source.ID, "b")
			case "sql_error":
				_, err = st.DB().ExecContext(ctx, `ALTER TABLE source_merge_archive_only_messages RENAME TO inventory_broken_table`)
				require.NoError(err)
			case "invalid_local_id":
				inventoryMessage(t, st, source.ID, "")
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			called := false
			result, err := collectGmailInventory(ctx, st, func(context.Context, *store.Source) (gmail.API, error) {
				called = true
				return nil, errors.New("must not reach provider")
			}, opts)
			require.Error(err)
			assert.Equal("inconclusive", result.Status)
			assert.False(result.LocalComplete)
			assert.False(called)
			assert.Nil(result.RemoteOnly)
			assert.Nil(result.LocalNotObserved)
		})
	}
}

// serial: daemon subprocess marker and synthetic DefaultTransport are global.
func TestDiagnoseGmailInventoryCommand(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	markDaemonCLISubprocessForTest(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
	require.NoError(err)
	inventoryMessage(t, st, source.ID, "d")
	partial := false
	factory := inventoryProvider(t, func(req *http.Request) (string, error) {
		if strings.HasSuffix(req.URL.Path, "/profile") {
			return inventoryProfile("100"), nil
		}
		next := ""
		if partial {
			next = "second"
		}
		return inventoryPage([]string{"c", "b"}, next), nil
	})
	dsn := inventoryFixtureDSN(t, st)
	deps := gmailInventoryDeps{openStore: func(ctx context.Context) (*store.Store, func(), error) {
		reader, openErr := store.OpenReadOnlyContext(ctx, dsn)
		if openErr != nil {
			return nil, nil, openErr
		}
		if schemaErr := reader.ValidateSchemaContext(ctx); schemaErr != nil {
			_ = reader.Close()
			return nil, nil, schemaErr
		}
		return reader, func() { _ = reader.Close() }, nil
	}, newClient: factory}
	for _, jsonOut := range []bool{false, true} {
		command := newDiagnoseGmailInventoryCmd(deps)
		command.SetContext(t.Context())
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(io.Discard)
		args := []string{"--source-id", strconv.FormatInt(source.ID, 10), "--limit", "1"}
		if jsonOut {
			args = append(args, "--json")
		}
		command.SetArgs(args)
		require.NoError(command.Execute())
		t.Logf("Synthetic JSON=%t output:\n%s", jsonOut, output.String())
		if jsonOut {
			var result gmailInventoryResult
			require.NoError(json.Unmarshal(output.Bytes(), &result))
			assert.Equal(1, result.RemoteOnly.Omitted)
		} else {
			assert.Contains(output.String(), "Gmail present, locally absent: 2\n  b\n  ... 1 IDs omitted")
			assert.Contains(output.String(), "may be retained history): 1\n  d")
		}
	}
	partial = true
	command := newDiagnoseGmailInventoryCmd(deps)
	command.SetContext(t.Context())
	var incomplete bytes.Buffer
	command.SetOut(&incomplete)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--source-id", strconv.FormatInt(source.ID, 10), "--max-pages", "1", "--json"})
	require.Error(command.Execute())
	var incompleteResult gmailInventoryResult
	require.NoError(json.Unmarshal(incomplete.Bytes(), &incompleteResult))
	assert.Equal("partial", incompleteResult.Status)
	assert.Nil(incompleteResult.LocalNotObserved)
	assert.Equal(1, incompleteResult.LocalUnassessed)
}

// serial: the daemon subprocess marker is global.
func TestDiagnoseGmailInventoryRejectsInvalidArguments(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing_source", nil},
		{"zero_source", []string{"--source-id", "0"}},
		{"negative_source", []string{"--source-id", "-1"}},
		{"negative_limit", []string{"--source-id", "1", "--limit", "-1"}},
		{"zero_timeout", []string{"--source-id", "1", "--timeout", "0s"}},
		{"negative_timeout", []string{"--source-id", "1", "--timeout", "-1s"}},
		{"zero_messages", []string{"--source-id", "1", "--max-messages", "0"}},
		{"negative_messages", []string{"--source-id", "1", "--max-messages", "-1"}},
		{"too_many_messages", []string{"--source-id", "1", "--max-messages", "10000001"}},
		{"zero_pages", []string{"--source-id", "1", "--max-pages", "0"}},
		{"negative_pages", []string{"--source-id", "1", "--max-pages", "-1"}},
		{"too_many_pages", []string{"--source-id", "1", "--max-pages", "1000001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var opened, clients int
			deps := gmailInventoryDeps{
				openStore: func(context.Context) (*store.Store, func(), error) {
					opened++
					return nil, nil, errors.New("unexpected archive open")
				},
				newClient: func(context.Context, *store.Source) (gmail.API, error) {
					clients++
					return nil, errors.New("unexpected Gmail client creation")
				},
			}
			command := newDiagnoseGmailInventoryCmd(deps)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			command.SetArgs(append(tc.args, "--json"))
			err := command.ExecuteContext(t.Context())
			require.Error(err)
			assert.Zero(opened, "invalid arguments must be rejected before archive access")
			assert.Zero(clients, "invalid arguments must be rejected before Gmail access")
			assert.NotContains(output.String(), "Gmail inventory for source", "usage help is allowed, an inventory report is not")
			assert.NotContains(output.String(), `"source_id":`, "invalid arguments must not emit a JSON inventory report")
			assert.NotContains(output.String(), `"status":`)
			assert.ErrorContains(err, "require a positive --source-id")
		})
	}
}

// serial: production Gmail clients use the synthetic DefaultTransport.
func TestGmailInventoryHumanOutputPreservesJSON(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason, explanation string
		maxPages                          int
	}{
		{"complete", "complete", "pagination_finished", "all Gmail pages read", 10},
		{"partial", "partial", "page_limit", "reached --max-pages", 1},
		{"inconclusive", "inconclusive", "mailbox_changed_or_history_unknown", "Gmail changed during listing or its mailbox change marker is unavailable", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
			require.NoError(err)
			inventoryMessage(t, st, source.ID, "retained")
			profiles := 0
			factory := inventoryProvider(t, func(req *http.Request) (string, error) {
				if strings.HasSuffix(req.URL.Path, "/profile") {
					profiles++
					if tc.name == "inconclusive" && profiles == 2 {
						return inventoryProfile("101"), nil
					}
					return inventoryProfile("100"), nil
				}
				if tc.name == "partial" {
					return inventoryPage([]string{"remote"}, "next"), nil
				}
				return inventoryPage([]string{"remote"}, ""), nil
			})
			opts := inventoryOptions(source.ID)
			opts.MaxPages = tc.maxPages
			result, err := collectGmailInventory(t.Context(), st, factory, opts)
			if tc.status == "complete" {
				require.NoError(err)
			} else {
				require.Error(err)
			}
			assert.Equal(tc.status, result.Status)
			assert.Equal(tc.reason, result.Reason)
			before, err := json.Marshal(result)
			require.NoError(err)
			var human bytes.Buffer
			require.NoError(writeGmailInventory(&human, result, false))
			assert.Contains(human.String(), fmt.Sprintf("Gmail inventory for source %d: %s (%s)\n", source.ID, tc.status, tc.explanation))
			assert.NotContains(human.String(), tc.reason)
			assert.Contains(human.String(), "Local present, not observed in Gmail (may be retained history)")
			assert.NotContains(human.String(), "Local-only IDs may be legitimate retained history.")
			assert.Contains(human.String(), "Matching message IDs do not verify saved original emails, attachments, extracted message data, search indexes or stored files.")
			assert.Contains(human.String(), "Archived message IDs: 1 (collection complete: true); Gmail IDs observed: 1")
			assert.NotContains(human.String(), "rows")
			assert.NotContains(human.String(), "identities")
			assert.NotContains(human.String(), "projections")
			assert.NotContains(human.String(), "FTS")
			if tc.status != "complete" {
				assert.Contains(human.String(), "Archived message IDs with unknown Gmail presence: 1")
			}
			assert.Contains(result.Scope, "Gmail messages, including Spam and Trash")
			assert.Contains(result.Scope, "hidden messages and those deleted from Gmail")
			assert.Contains(result.Scope, "history retained only from merged accounts is excluded")
			assert.NotContains(result.Scope, "canonical identities")
			assert.NotContains(result.Scope, "rows")
			var machine bytes.Buffer
			require.NoError(writeGmailInventory(&machine, result, true))
			var decoded gmailInventoryResult
			require.NoError(json.Unmarshal(machine.Bytes(), &decoded))
			assert.Equal(result, decoded)
			assert.Equal(tc.reason, decoded.Reason, "JSON keeps the machine-readable reason")
			after, err := json.Marshal(result)
			require.NoError(err)
			assert.Equal(string(before), string(after), "human rendering must not mutate the JSON result")
		})
	}
}

func inventoryArchiveState(t *testing.T, st *store.Store) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for _, table := range []string{"sources", "source_settings", "messages", "message_bodies", "message_raw", "attachments", "sync_runs", "sync_run_items"} {
		rows, err := st.DB().QueryContext(t.Context(), "SELECT * FROM "+table)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			require.NoError(t, rows.Scan(dest...))
			result[table] = append(result[table], fmt.Sprint(values))
		}
		require.NoError(t, rows.Err())
	}
	return result
}

func inventoryFixtureDSN(t *testing.T, st *store.Store) string {
	t.Helper()
	if st.IsPostgreSQL() {
		dsn, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		var database, schema string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT current_database(), current_schema()").Scan(&database, &schema))
		dsn.Path = "/" + database
		query := dsn.Query()
		query.Set("search_path", schema+",public")
		dsn.RawQuery = query.Encode()
		return dsn.String()
	}
	var seq int
	var name, path string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &path))
	return path
}

// serial: production Gmail clients use the synthetic DefaultTransport.
func TestGmailInventoryProfileFailures(t *testing.T) {
	for _, mode := range []string{"before_error", "before_mismatch", "after_error", "after_mismatch", "unknown_history", "client_error"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(sourceTypeGmail, "inventory@example.test")
			require.NoError(err)
			inventoryMessage(t, st, source.ID, "local")
			profiles := 0
			factory := inventoryProvider(t, func(req *http.Request) (string, error) {
				if !strings.HasSuffix(req.URL.Path, "/profile") {
					return inventoryPage([]string{"remote"}, ""), nil
				}
				profiles++
				if mode == "before_error" || mode == "after_error" && profiles == 2 {
					return "{", nil
				}
				if mode == "before_mismatch" || mode == "after_mismatch" && profiles == 2 {
					return strings.ReplaceAll(inventoryProfile("100"), "inventory@example.test", "different@example.test"), nil
				}
				if mode == "unknown_history" {
					return inventoryProfile("0"), nil
				}
				return inventoryProfile("100"), nil
			})
			if mode == "client_error" {
				factory = func(context.Context, *store.Source) (gmail.API, error) {
					return nil, errors.New("synthetic credential failure")
				}
			}
			result, err := collectGmailInventory(t.Context(), st, factory, inventoryOptions(source.ID))
			require.Error(err)
			assert.Equal("inconclusive", result.Status)
			assert.True(result.LocalComplete)
			assert.Nil(result.LocalNotObserved)
			assert.Equal(1, result.LocalUnassessed)
		})
	}
}
