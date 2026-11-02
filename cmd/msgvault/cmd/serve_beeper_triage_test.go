package cmd

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperTriageLocalAndArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	id, err := st.EnsureConversationWithType(src.ID, "!fixture:example.test", "direct_chat", "Fixture")
	require.NoError(err)
	activity := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	posts := 0
	archived := false
	unavailable := false
	mergedReadback := false
	mergedObservation := false
	requests := 0
	capabilities := `{"archive":true}`
	nativeActivity := activity
	account := "signal"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if unavailable {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			posts++
			archived = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			body := `{"items":[],"hasMore":false,"oldestCursor":"","newestCursor":""}`
			if r.URL.Query().Get("cursor") == "" {
				body = fmt.Sprintf(`{"items":[{"id":"head","chatID":"!fixture:example.test","accountID":%q,"timestamp":%q,"type":"TEXT","text":"Synthetic reviewed request"}],"hasMore":false,"oldestCursor":"head","newestCursor":"head"}`, account, nativeActivity)
			}
			_, writeErr := io.WriteString(w, body)
			assert.NoError(writeErr)
			return
		}
		merged := ""
		if mergedObservation || (mergedReadback && posts >= 2) {
			merged = `,"mergedIntoChatID":"another"`
		}
		_, err := fmt.Fprintf(w, `{"id":"!fixture:example.test","accountID":%q,"lastActivity":%q,"isArchived":%t,"capabilities":%s,"labels":[{"id":"native-1","name":"Native label"}],"draft":{"text":"NATIVE-COMPOSER-BODY"}%s}`, account, nativeActivity, archived, capabilities, merged)
		assert.NoError(err)
	}))
	defer provider.Close()
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	cfg.Beeper.URL = provider.URL
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}
	run := func(args ...string) (map[string]any, error) {
		t.Helper()
		var out map[string]any
		err := adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: append(append([]string{"beeper"}, args...), "--json")}, func(e api.CLIRunEvent) error {
			if e.Type == "stdout" {
				return json.Unmarshal([]byte(e.Data), &out)
			}
			return nil
		})
		return out, err
	}
	for _, invalid := range [][]string{
		{"classify", "--conversation", strconv.FormatInt(id, 10), "--category", "todo", "--needs-answer=1", "--apply"},
		{"classify", "--conversation", strconv.FormatInt(id, 10), "--clear", "--category", "todo", "--apply"},
		{"archive", "--conversation", strconv.FormatInt(id, 10), "--revision", "0", "--apply"},
		{"archive", "--conversation", strconv.FormatInt(id, 10), "--reconcile", "--revision", "0", "--policy", "archive-selected", "--apply"},
	} {
		_, err := parseBeeperArgs(append([]string{"beeper"}, invalid...))
		require.Error(err, "invalid controls must be rejected by the parser")
	}
	preview, err := run("classify", "--conversation", strconv.FormatInt(id, 10), "--category", "todo")
	require.NoError(err)
	assert.IsType("", preview["observed_activity"])
	assert.Empty(preview["observed_activity"], "offline preview must disclose its omitted activity evidence")
	assert.IsType("", preview["context_stamp"])
	assert.Empty(preview["context_stamp"], "preview must disclose replacement review metadata")
	assert.Contains(preview, "reviewed_head", "an explicit null distinguishes clearing evidence from an omitted preview field")
	assert.Nil(preview["reviewed_head"])
	for _, flags := range [][]string{nil, {"--json=false"}, {"--json"}} {
		args := append([]string{"beeper", "classify", "--conversation", strconv.FormatInt(id, 10), "--category", "todo"}, flags...)
		var compatible map[string]any
		err := adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: args}, func(e api.CLIRunEvent) error {
			if e.Type == "stdout" {
				return json.Unmarshal([]byte(e.Data), &compatible)
			}
			return nil
		})
		require.NoError(err)
		assert.Equal(preview, compatible, "JSON output is independent of the compatibility flag")
	}
	row, err := st.GetConversationGTDContext(t.Context(), id)
	require.NoError(err)
	assert.Zero(row.Revision)
	_, err = run("classify", "--conversation", strconv.FormatInt(id, 10), "--category", "todo", "--observed-activity", activity, "--apply")
	require.NoError(err)
	row, err = st.GetConversationGTDContext(t.Context(), id)
	require.NoError(err)
	assert.True(row.NeedsAnswer)
	assert.Zero(requests, "local classification must work without any token or provider read")
	no, yesAnswerFlag := false, true
	for _, change := range []struct {
		category string
		override *bool
		want     bool
	}{
		{"waiting", nil, true},
		{"waiting", &no, false},
		{"todo", nil, false},
		{"reference", &yesAnswerFlag, true},
		{"todo", nil, true},
	} {
		args := []string{"classify", "--conversation", strconv.FormatInt(id, 10), "--category", change.category, "--revision", strconv.FormatInt(row.Revision, 10), "--observed-activity", activity}
		if change.override != nil {
			args = append(args, "--needs-answer="+strconv.FormatBool(*change.override))
		}
		preview, err := run(args...)
		require.NoError(err)
		assert.Equal(change.want, preview["needs_answer"], "preview category %s", change.category)
		unchanged, err := st.GetConversationGTDContext(t.Context(), id)
		require.NoError(err)
		assert.Equal(row, unchanged, "classification preview cannot alter independent state")
		applied, err := run(append(args, "--apply")...)
		require.NoError(err)
		assert.Equal(change.want, applied["needs_answer"], "applied category %s", change.category)
		row, err = st.GetConversationGTDContext(t.Context(), id)
		require.NoError(err)
	}
	resetPreview, err := run("classify", "--conversation", strconv.FormatInt(id, 10), "--clear")
	require.NoError(err)
	assert.Equal(false, resetPreview["needs_answer"])
	assert.Zero(requests, "recategorization and preview must stay local")
	_, err = run("classify", "--conversation", strconv.FormatInt(id, 10), "--category", "reference", "--revision", strconv.FormatInt(row.Revision-1, 10))
	require.ErrorIs(err, store.ErrConversationGTDRevision, "stale preview must refuse")
	unchanged, err := st.GetConversationGTDContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(row, unchanged)
	require.NoError(beeper.SaveToken(cfg.TokensDir(), "test-token"))
	view, err := run("triage", "--source-id", strconv.FormatInt(src.ID, 10), "--refresh")
	require.NoError(err)
	viewRows, ok := view["conversations"].([]any)
	require.True(ok, "conversations must be an array")
	require.Len(viewRows, 1)
	viewRow, ok := viewRows[0].(map[string]any)
	require.True(ok, "conversation must be an object")
	nativeView, ok := viewRow["native"].(map[string]any)
	require.True(ok, "native observation must be an object")
	assert.NotContains(nativeView, "draft")
	assert.NotContains(string(mustJSON(t, view)), "NATIVE-COMPOSER-BODY")
	assert.Equal([]any{map[string]any{"id": "native-1", "name": "Native label"}}, nativeView["labels"])
	for _, observation := range []string{"merged", "foreign"} {
		mergedObservation = observation == "merged"
		account = "signal"
		if observation == "foreign" {
			account = "other-account"
		}
		view, err = run("triage", "--source-id", strconv.FormatInt(src.ID, 10), "--refresh")
		require.NoError(err)
		mergedRows, ok := view["conversations"].([]any)
		require.True(ok)
		require.Len(mergedRows, 1)
		mergedView, ok := mergedRows[0].(map[string]any)
		require.True(ok)
		assert.NotContains(mergedView, "native")
		assert.Equal("identity_mismatch", mergedView["freshness"], observation)
	}
	mergedObservation, account = false, "signal"
	packet, err := adapter.runBeeperContextSnapshot(t.Context(), id, beeper.DefaultContextBounds(), nil, true)
	require.NoError(err)
	require.True(packet.Complete, packet.Reason)
	reviewedHead := jsontext.Value(mustJSON(t, packet.Head))
	row, err = st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: row.Category, ObservedActivity: activity, ContextStamp: packet.Stamp, ReviewedHead: reviewedHead})
	require.NoError(err)
	out, err := run("archive", "--conversation", strconv.FormatInt(id, 10), "--observed-activity", activity, "--revision", strconv.FormatInt(row.Revision, 10), "--policy", "archive-selected")
	require.NoError(err)
	assert.Equal("preview", out["status"])
	assert.Zero(posts)
	previewed, err := st.GetConversationGTDContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(row.Revision, previewed.Revision)
	assert.False(previewed.ArchiveHold)
	archiveArgs := []string{"archive", "--conversation", strconv.FormatInt(id, 10), "--observed-activity", activity, "--revision", strconv.FormatInt(row.Revision, 10), "--apply"}
	out, err = run(archiveArgs...)
	require.NoError(err)
	assert.Equal("retained", out["status"])
	assert.Zero(posts)
	yesAnswer := true
	row, err = st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: "reference", NeedsAnswer: &yesAnswer, ObservedActivity: activity, ContextStamp: packet.Stamp, ReviewedHead: reviewedHead})
	require.NoError(err)
	archiveArgs = []string{"archive", "--conversation", strconv.FormatInt(id, 10), "--observed-activity", activity, "--revision", strconv.FormatInt(row.Revision, 10), "--apply"}
	out, err = run(archiveArgs...)
	require.NoError(err)
	assert.Equal("retained", out["status"])
	assert.Zero(posts, "answer-needed reference is actionable")
	for _, caps := range []string{`null`, `{}`, `{"archive":false}`} {
		capabilities = caps
		_, err = run(append(archiveArgs, "--policy", "archive-selected")...)
		require.Error(err)
		assert.Zero(posts)
	}
	capabilities = `{"archive":true}`
	nativeActivity = "2026-11-01T04:00:00Z"
	_, err = run(append(archiveArgs, "--policy", "archive-selected")...)
	require.Error(err)
	assert.Zero(posts)
	nativeActivity = activity
	account = "foreign-account"
	_, err = run(append(archiveArgs, "--policy", "archive-selected")...)
	require.Error(err)
	assert.Zero(posts)
	account = "signal"
	out, err = run(append(archiveArgs, "--policy", "archive-selected")...)
	require.NoError(err)
	assert.Equal("archived", out["status"])
	assert.Equal(1, posts)
	archived = false
	mergedReadback = true
	out, err = run(append(archiveArgs, "--policy", "archive-selected")...)
	require.NoError(err)
	assert.Equal("unknown", out["status"])
	row, err = st.GetConversationGTDContext(t.Context(), id)
	require.NoError(err)
	assert.True(row.ArchiveHold)
	mergedReadback = false
	archived = false
	require.NoError(st.SetConversationGTDHoldContext(t.Context(), id, row.Revision, false))
	_, err = run("archive", "--conversation", strconv.FormatInt(id, 10), "--revision", strconv.FormatInt(row.Revision, 10), "--reconcile", "--apply")
	require.NoError(err)
	assert.Equal(2, posts, "cleared hold and now-unarchived state cannot turn reconciliation into POST")
	archived = true
	require.NoError(st.SetConversationGTDHoldContext(t.Context(), id, row.Revision, true))
	out, err = run(append(archiveArgs, "--policy", "archive-selected")...)
	require.NoError(err)
	assert.Equal("already_archived", out["status"])
	assert.Equal(2, posts)
	_, err = run(append(archiveArgs, "--policy", "archive-selected")...)
	require.NoError(err)
	assert.Equal(2, posts)
	unavailable = true
	out, err = run("triage", "--source-id", strconv.FormatInt(src.ID, 10), "--needs-answer", "--refresh")
	require.NoError(err)
	rows, ok := out["conversations"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	unavailableRow, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.Equal("unavailable", unavailableRow["freshness"])
	unavailable = false
	yes := true
	_, err = st.UpdateSourceSettingsContext(t.Context(), src.ID, store.SourceSettingsUpdate{HistoryOnly: &yes})
	require.NoError(err)
	beforeRequests := requests
	out, err = run("triage", "--source-id", strconv.FormatInt(src.ID, 10), "--refresh")
	require.NoError(err)
	assert.Equal(beforeRequests, requests)
	ineligibleRows, ok := out["conversations"].([]any)
	require.True(ok)
	require.Len(ineligibleRows, 1)
	ineligibleRow, ok := ineligibleRows[0].(map[string]any)
	require.True(ok)
	assert.Equal("source_ineligible", ineligibleRow["freshness"])
	err = adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: []string{"beeper", "triage", "--source-id", strconv.FormatInt(src.ID, 10)}, Grant: &agentgrant.Grant{}}, nil)
	require.Error(err)
}

// Activity alone does not prove a reviewed packet. A body edit to an older
// selected event must block archive even when native activity and head agree.
func TestBeeperArchiveRequiresReviewedPacket(t *testing.T) {
	for _, mode := range []string{"timestamp only", "same activity older edit", "reviewed unchanged", "initial window one", "wrong initial window", "original delta lower", "delta without original lower", "edited linked parent", "stored future head", "required retained removed", "required retained replaced", "required retained matching", "required retained disabled", "required early archived removed", "required early archived replaced", "required early archived matching", "required early archived disabled", "required final archived removed", "required final archived replaced", "required final archived matching", "required final archived disabled"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "signal")
			require.NoError(err)
			id, err := st.EnsureConversation(source.ID, "reviewed-chat", "Reviewed fixture")
			require.NoError(err)
			activity := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
			messages := []beeper.Message{
				{ID: "older", ChatID: "reviewed-chat", AccountID: "signal", Timestamp: activity.Add(-time.Minute), Type: "TEXT", Text: "Original older question"},
				{ID: "head", ChatID: "reviewed-chat", AccountID: "signal", Timestamp: activity, Type: "TEXT", Text: "Unchanged newest event"},
			}
			var mu sync.Mutex
			posts, archived := 0, false
			nativeWrites := 0
			archiveReads := 0
			archivePhase := false
			var composer any
			draftCase := strings.HasPrefix(mode, "required ")
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if archivePhase && r.Method != http.MethodGet {
					nativeWrites++
				}
				var body any
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/chats/reviewed-chat/archive":
					posts++
					archived = true
					w.WriteHeader(http.StatusNoContent)
					return
				case r.Method == http.MethodGet && r.URL.Path == "/v1/chats/reviewed-chat":
					if archivePhase {
						archiveReads++
						if strings.HasPrefix(mode, "required final archived ") && archiveReads == 4 {
							archived = true // P, context before/after, then final F observation.
							if strings.HasSuffix(mode, " removed") {
								composer = nil
							} else if strings.HasSuffix(mode, " replaced") {
								composer = map[string]string{"text": "Synthetic external replacement"}
							}
						}
					}
					body = map[string]any{"id": "reviewed-chat", "accountID": "signal", "title": "Reviewed fixture", "lastActivity": activity, "isArchived": archived, "capabilities": map[string]bool{"archive": true}, "draft": composer}
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/chats/reviewed-chat/messages/"):
					for _, message := range messages {
						if message.ID == strings.TrimPrefix(r.URL.Path, "/v1/chats/reviewed-chat/messages/") {
							body = message
						}
					}
					if body == nil {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				case r.Method == http.MethodGet && r.URL.Path == "/v1/chats/reviewed-chat/messages":
					body = map[string]any{"items": []beeper.Message{}, "hasMore": false, "oldestCursor": "", "newestCursor": ""}
					if r.URL.Query().Get("cursor") == "" {
						selected := append([]beeper.Message(nil), messages...)
						for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
							selected[left], selected[right] = selected[right], selected[left]
						}
						body = map[string]any{"items": selected, "hasMore": false, "oldestCursor": messages[0].ID, "newestCursor": messages[len(messages)-1].ID}
					} else if r.URL.Query().Get("direction") == "after" && r.URL.Query().Get("cursor") == "older" && len(messages) > 1 {
						body = map[string]any{"items": []beeper.Message{messages[1]}, "hasMore": true, "oldestCursor": "head", "newestCursor": "head"}
					}
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
				encoded, encodeErr := json.Marshal(body)
				if encodeErr != nil {
					assert.Fail("synthetic provider encoding failed")
					http.Error(w, "fixture encoding failed", http.StatusInternalServerError)
					return
				}
				_, writeErr := w.Write(encoded)
				assert.NoError(writeErr)
			}))
			defer provider.Close()
			home := t.TempDir()
			cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
			cfg.Beeper.URL = provider.URL
			cfg.Beeper.RateLimitQPS = 1000
			require.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-review-token"))
			adapter := &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}
			change := store.ConversationGTDChange{Category: "reference", ObservedActivity: activity.Format(time.RFC3339Nano)}
			archiveScope := []string{}
			if mode != "timestamp only" {
				bounds := beeper.DefaultContextBounds()
				var lower *beeper.ContextAnchor
				if mode == "initial window one" || mode == "wrong initial window" || mode == "edited linked parent" {
					bounds.InitialMessages = 1
				}
				if mode == "edited linked parent" {
					messages[1].LinkedMessageID = "older"
				}
				if mode == "original delta lower" || mode == "delta without original lower" {
					allMessages, latestActivity := messages, activity
					messages, activity = messages[:1], messages[0].Timestamp
					previous, previousErr := adapter.runBeeperContextSnapshot(t.Context(), id, bounds, nil, true)
					require.NoError(previousErr)
					require.True(previous.Complete, previous.Reason)
					lower = &previous.Head
					messages, activity = allMessages, latestActivity
					if mode == "original delta lower" {
						archiveScope = []string{"--lower-anchor-json", string(mustJSON(t, lower))}
					}
				}
				if mode == "initial window one" || mode == "edited linked parent" {
					archiveScope = []string{"--initial-review", "--initial-context-messages", "1"}
				}
				packet, captureErr := adapter.runBeeperContextSnapshot(t.Context(), id, bounds, lower, lower == nil)
				require.NoError(captureErr)
				require.True(packet.Complete, packet.Reason)
				if mode == "initial window one" || mode == "wrong initial window" {
					require.Len(packet.Messages, 1)
				} else {
					require.Len(packet.Messages, 2)
				}
				head, encodeErr := json.Marshal(packet.Head)
				require.NoError(encodeErr)
				if mode == "stored future head" {
					var future map[string]any
					require.NoError(json.Unmarshal(head, &future))
					future["future_encoding"] = true
					head, encodeErr = json.Marshal(future)
					require.NoError(encodeErr)
				}
				change.ContextStamp = packet.Stamp
				change.ReviewedHead = jsontext.Value(head)
				if mode == "same activity older edit" || mode == "edited linked parent" {
					mu.Lock()
					messages[0].Text = "Edited older question at the same native activity"
					mu.Unlock()
					edited, captureErr := adapter.runBeeperContextSnapshot(t.Context(), id, bounds, lower, lower == nil)
					require.NoError(captureErr)
					require.True(edited.Complete, edited.Reason)
					assert.Equal(packet.Activity, edited.Activity)
					assert.Equal(packet.Head, edited.Head, "the edit is not the newest event")
					assert.NotEqual(packet.Stamp, edited.Stamp, "the real context digest must observe the older edit")
				}
			}
			var managed store.BeeperDraft
			if draftCase {
				if strings.HasPrefix(mode, "required retained ") {
					change.Category = "todo"
				}
				managed, err = st.CreateBeeperDraftContext(t.Context(), source.ID, "reviewed-chat", "Synthetic managed answer")
				require.NoError(err)
				managed, err = st.FinishBeeperDraftContext(t.Context(), managed.DraftID, managed.Revision, "Synthetic managed answer")
				require.NoError(err)
				if !strings.HasSuffix(mode, " disabled") {
					archiveScope = append(archiveScope, "--required-draft-id", managed.DraftID, "--required-draft-revision", strconv.FormatInt(managed.Revision, 10))
				}
				mu.Lock()
				if strings.HasSuffix(mode, " matching") || (strings.HasPrefix(mode, "required final archived ") && !strings.HasSuffix(mode, " disabled")) {
					composer = map[string]string{"text": "Synthetic managed answer"}
				} else if strings.HasSuffix(mode, " replaced") {
					composer = map[string]string{"text": "Synthetic external replacement"}
				}
				archived = strings.HasPrefix(mode, "required early archived ")
				archivePhase = true
				mu.Unlock()
			}
			saved, err := st.SetConversationGTDContext(t.Context(), id, change)
			require.NoError(err)
			var out map[string]any
			err = adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: append([]string{"beeper", "archive", "--conversation", strconv.FormatInt(id, 10), "--revision", strconv.FormatInt(saved.Revision, 10), "--observed-activity", change.ObservedActivity, "--apply", "--json"}, archiveScope...)}, func(event api.CLIRunEvent) error {
				return json.Unmarshal([]byte(event.Data), &out)
			})
			mu.Lock()
			actualPosts, actualWrites, actualReads := posts, nativeWrites, archiveReads
			mu.Unlock()
			if draftCase {
				if strings.HasPrefix(mode, "required final archived ") {
					assert.Equal(4, actualReads, "the draft changes only at the final archived observation")
				}
				require.NoError(err)
				assert.Zero(actualPosts, "an early outcome must not archive")
				assert.Zero(actualWrites, "reuse must not replace, delete or send the native composer")
				status := "draft_held"
				if strings.HasSuffix(mode, " matching") || strings.HasSuffix(mode, " disabled") {
					status = "already_archived"
					if strings.HasPrefix(mode, "required retained ") {
						status = "retained"
					}
				}
				assert.Equal(status, out["status"], "requested draft equality must precede every successful early outcome")
				currentDraft, readErr := st.GetBeeperDraftContext(t.Context(), managed.DraftID)
				require.NoError(readErr)
				assert.Equal(managed, currentDraft, "reuse must preserve the managed ID, revision, text and pending state")
				current, readErr := st.GetConversationGTDContext(t.Context(), id)
				require.NoError(readErr)
				assert.Equal(saved, current, "an early outcome must preserve classification, answer-needed and hold state")
			} else if mode == "reviewed unchanged" || mode == "initial window one" || mode == "original delta lower" {
				require.NoError(err)
				assert.Equal(1, actualPosts, "an unchanged reviewed packet must still archive once")
				assert.Equal("archived", out["status"])
			} else {
				assert.Zero(actualPosts, "timestamp-only or edited content must never authorize a POST")
				assert.NotEqual("archived", out["status"])
			}
		})
	}
}

func TestBeeperLoggingPassthroughFromCobra(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	var providerRequests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	cfg.Beeper.URL = provider.URL
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}
	root := &cobra.Command{Use: "msgvault", SilenceUsage: true, SilenceErrors: true}
	root.SetContext(t.Context())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.PersistentFlags().String("log-level", "", "")
	root.PersistentFlags().Bool("verbose", false, "")
	root.PersistentFlags().Bool("log-sql", false, "")
	root.PersistentFlags().Int64("log-sql-slow-ms", 0, "")
	group := newBeeperCommand()
	root.AddCommand(group)
	leaf, _, err := root.Find([]string{"beeper", "triage"})
	require.NoError(err)
	var serialized []string
	var output map[string]any
	leaf.RunE = func(cmd *cobra.Command, args []string) error {
		var serializeErr error
		serialized, serializeErr = daemonCLIArgsFromCobra(cmd, args)
		if serializeErr != nil {
			return serializeErr
		}
		return adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: serialized}, func(event api.CLIRunEvent) error {
			return json.Unmarshal([]byte(event.Data), &output)
		})
	}
	root.SetArgs([]string{"--log-level", "debug", "--verbose", "--log-sql", "--log-sql-slow-ms", "250", "beeper", "triage", "--source-id", strconv.FormatInt(source.ID, 10), "--json"})
	err = root.Execute()
	require.NoError(err, "the actual serialized logging intent must reach the in-process Beeper handler")
	for _, flag := range []string{"--log-level=debug", "--verbose", "--log-sql", "--log-sql-slow-ms=250"} {
		assert.Contains(serialized, flag)
	}
	assert.Equal([]any{}, output["conversations"])
	assert.Zero(providerRequests.Load(), "local triage must not contact Beeper")
	_, err = parseBeeperArgs([]string{"beeper", "triage", "--source-id", strconv.FormatInt(source.ID, 10), "--not-a-supported-flag"})
	assert.Error(err, "unknown flags must remain refused")
}

func TestBeeperCatalogFallibleIdentitiesAndContinuation(t *testing.T) {
	st := testutil.NewTestStore(t)
	setup := require.New(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	setup.NoError(err)
	foreignSource, err := st.GetOrCreateSource("beeper", "other-account")
	setup.NoError(err)
	known, err := st.EnsureConversation(source.ID, "known-chat", "Known fixture")
	setup.NoError(err)
	later, err := st.EnsureConversation(source.ID, "later-chat", "Later fixture")
	setup.NoError(err)
	_, err = st.EnsureConversation(foreignSource.ID, "foreign-chat", "Foreign fixture")
	setup.NoError(err)
	var writes atomic.Int64
	var catalogReads atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		catalogReads.Add(1)
		if r.URL.Path != "/v1/chats/search" || r.URL.Query().Get("accountIDs") != "signal" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body any
		switch cursor := r.URL.Query().Get("cursor"); cursor {
		case "":
			body = map[string]any{"items": []any{map[string]any{"id": "known-chat", "accountID": "signal"}, map[string]any{"id": "missing-account"}, map[string]any{"id": "foreign-chat", "accountID": "other-account"}}, "hasMore": true, "oldestCursor": "page-two"}
		case "page-two":
			body = map[string]any{"items": []any{map[string]any{"id": "another-missing-account"}, map[string]any{"id": "foreign-chat", "accountID": "other-account"}}, "hasMore": true, "oldestCursor": "page-three"}
		case "page-three":
			body = map[string]any{"items": []any{map[string]any{"id": "later-chat", "accountID": "signal"}}, "hasMore": false, "oldestCursor": "end"}
		case "duplicate-only":
			body = map[string]any{"items": []any{map[string]any{"id": "known-chat", "accountID": "signal"}, map[string]any{"id": "known-chat", "accountID": "signal"}}, "hasMore": true, "oldestCursor": "page-three"}
		case "stuck":
			body = map[string]any{"items": []any{map[string]any{"id": "known-chat", "accountID": "signal"}}, "hasMore": true, "oldestCursor": "stuck"}
		default:
			body = map[string]any{"items": nil, "hasMore": false}
		}
		encoded, encodeErr := json.Marshal(body)
		if encodeErr != nil {
			http.Error(w, "fixture encoding failed", http.StatusInternalServerError)
			return
		}
		_, writeErr := w.Write(encoded)
		assert.NoError(t, writeErr)
	}))
	defer provider.Close()
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	cfg.Beeper.URL = provider.URL
	setup.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-catalog-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}
	run := func(t *testing.T, cursor string) (map[string]any, error) {
		t.Helper()
		args := []string{"beeper", "triage", "--source-id", strconv.FormatInt(source.ID, 10), "--catalog", "--json"}
		if cursor != "" {
			args = append(args, "--catalog-cursor", cursor)
		}
		var out map[string]any
		err := adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
			decoder := json.NewDecoder(strings.NewReader(event.Data))
			decoder.UseNumber()
			return decoder.Decode(&out)
		})
		return out, err
	}
	for _, controls := range [][]string{{"--limit", "1"}, {"--limit", "50"}, {"--refresh"}, {"--refresh=false"}, {"--after", "0"}, {"--after", "1"}, {"--needs-answer"}, {"--needs-answer=false"}} {
		t.Run("explicit local controls "+strings.Join(controls, " ")+" are refused for native catalog", func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			before := catalogReads.Load()
			args := append([]string{"beeper", "triage", "--source-id", strconv.FormatInt(source.ID, 10), "--catalog"}, controls...)
			err := adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: args}, func(api.CLIRunEvent) error { return nil })
			assert.Equal(before, catalogReads.Load(), "reject unsupported local controls before provider access")
			require.Error(err, "native catalog cannot silently ignore explicit local triage controls")
		})
	}
	t.Run("mixed page retains usable source identities", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		out, err := run(t, "")
		require.NoError(err)
		assert.Equal([]any{map[string]any{"chat_id": "known-chat", "conversation_id": json.Number(strconv.FormatInt(known, 10))}}, out["catalog"])
		assert.Equal(json.Number("2"), out["omitted_count"])
		assert.Equal(true, out["coverage_incomplete"])
		assert.Equal(true, out["has_more"])
		assert.Equal("page-two", out["next_cursor"])
	})
	t.Run("all skipped page still advances to later valid identity", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		skipped, err := run(t, "page-two")
		require.NoError(err)
		assert.Equal([]any{}, skipped["catalog"])
		assert.Equal(json.Number("2"), skipped["omitted_count"])
		assert.Equal(true, skipped["coverage_incomplete"])
		assert.Equal(true, skipped["has_more"])
		require.Equal("page-three", skipped["next_cursor"])
		out, err := run(t, "page-three")
		require.NoError(err)
		assert.Equal([]any{map[string]any{"chat_id": "later-chat", "conversation_id": json.Number(strconv.FormatInt(later, 10))}}, out["catalog"])
		assert.Equal(false, out["has_more"])
	})
	t.Run("duplicate valid identities retain complete coverage and continuation", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		out, err := run(t, "duplicate-only")
		require.NoError(err)
		assert.Equal([]any{map[string]any{"chat_id": "known-chat", "conversation_id": json.Number(strconv.FormatInt(known, 10))}}, out["catalog"])
		assert.Equal(json.Number("0"), out["omitted_count"], "the valid identity is already represented, not missing")
		assert.Equal(false, out["coverage_incomplete"], "a duplicate valid entry alone does not lose coverage")
		assert.Equal(true, out["has_more"])
		require.Equal("page-three", out["next_cursor"])
		next, err := run(t, "page-three")
		require.NoError(err)
		assert.Equal([]any{map[string]any{"chat_id": "later-chat", "conversation_id": json.Number(strconv.FormatInt(later, 10))}}, next["catalog"])
		assert.Equal(false, next["has_more"])
	})
	for _, cursor := range []string{"stuck", "malformed"} {
		t.Run(cursor, func(t *testing.T) {
			_, err := run(t, cursor)
			require.Error(t, err, "provider malformed or stuck pages must remain refused")
		})
	}
	setup.Zero(writes.Load(), "catalog observation must never write a provider")
}

// A provider setup hold is still a successful context packet and must obey
// the same serialized byte cap as a packet produced by CaptureContext.
func TestBeeperContextMissingTokenEnvelopeBound(t *testing.T) {
	setup := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	setup.NoError(err)
	id, err := st.EnsureConversation(source.ID, "missing-token-chat", "Missing token fixture")
	setup.NoError(err)
	before, err := st.GetConversationGTDContext(t.Context(), id)
	setup.NoError(err)
	var requests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	cfg.Beeper.URL = provider.URL
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}
	for _, tc := range []struct {
		name      string
		maxBytes  int
		wantError bool
	}{{"normal held result", 128 << 10, false}, {"one byte cannot fit metadata", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var output strings.Builder
			err := adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: []string{
				"beeper", "context", "--conversation", strconv.FormatInt(id, 10),
				"--max-bytes", strconv.Itoa(tc.maxBytes), "--json",
			}}, func(event api.CLIRunEvent) error {
				if event.Type == "stdout" {
					if _, writeErr := output.WriteString(event.Data); writeErr != nil {
						return fmt.Errorf("collect context fixture output: %w", writeErr)
					}
				}
				return nil
			})
			payload := strings.TrimSuffix(output.String(), "\n")
			assert.Zero(requests.Load(), "missing credentials must never reach the provider")
			after, readErr := st.GetConversationGTDContext(t.Context(), id)
			require.NoError(readErr)
			assert.Equal(before, after, "held context cannot mutate local classification or archive state")
			if err == nil {
				assert.LessOrEqual(len(payload), tc.maxBytes, "every successfully emitted packet must fit the requested byte cap")
			}
			if tc.wantError {
				require.Error(err, "a one-byte cap cannot fit context result metadata")
				assert.Empty(payload, "an oversized packet cannot be emitted as success")
				return
			}
			require.NoError(err)
			var held map[string]any
			require.NoError(json.Unmarshal([]byte(payload), &held))
			assert.Equal("provider_unavailable", held["reason"])
			assert.Equal(false, held["complete"])
		})
	}
}

func TestBeeperTriageNativeLabelPresence(t *testing.T) {
	setup := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	setup.NoError(err)
	id, err := st.EnsureConversation(source.ID, "label-chat", "Synthetic label observation")
	setup.NoError(err)
	needsAnswer := true
	before, err := st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: "waiting", NeedsAnswer: &needsAnswer})
	setup.NoError(err)
	cases := []struct {
		name, wire string
		unknown    bool
		count      int
		oversized  bool
	}{
		{"omitted", "", true, 0, false},
		{"null", "null", true, 0, false},
		{"observed empty", "[]", false, 0, false},
		{"observed label", `[{"id":"label-1","name":"Answer"}]`, false, 1, false},
		{"oversized observation", `[{"id":"label-1","name":"` + strings.Repeat("x", (1<<20)+1) + `"}]`, false, 0, true},
	}
	var mode, catalogReads atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Equal(http.MethodGet, r.Method)
		if r.URL.Path == "/v1/labels" {
			catalogReads.Add(1)
			checks.NoError(json.NewEncoder(w).Encode([]beeper.Label{{ID: "label-1", Name: "Answer"}}))
			return
		}
		checks.Equal("/v1/chats/label-chat", r.URL.Path)
		tc := cases[mode.Load()]
		body := map[string]any{"id": "label-chat", "accountID": "signal", "isArchived": false, "capabilities": map[string]any{"archive": true}}
		if tc.wire != "" {
			body["labels"] = json.RawMessage(tc.wire)
		}
		// The bounded client may close an intentionally oversized response early.
		if err := json.NewEncoder(w).Encode(body); err != nil && !tc.oversized {
			checks.Fail("encode synthetic chat response", "%v", err)
		}
	}))
	defer provider.Close()
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	cfg.Beeper.URL = provider.URL
	cfg.Beeper.Accounts = []string{"signal", "labels-history", "labels-retired", "labels-destination"}
	setup.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-label-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}
	run := func(out any, args ...string) error {
		return adapter.runCLIBeeper(t.Context(), api.CLIRunRequest{Args: append([]string{"beeper"}, args...)}, func(event api.CLIRunEvent) error {
			return json.Unmarshal([]byte(event.Data), out)
		})
	}
	var catalog struct {
		Scope             string         `json:"scope"`
		NativeLabelWrites string         `json:"native_label_writes"`
		Labels            []beeper.Label `json:"labels"`
	}
	setup.NoError(run(&catalog, "labels", "--source-id", strconv.FormatInt(source.ID, 10)))
	checks.Equal("beeper_installation", catalog.Scope)
	checks.Equal("unsupported", catalog.NativeLabelWrites)
	checks.Equal([]beeper.Label{{ID: "label-1", Name: "Answer"}}, catalog.Labels)
	checks.Equal(int64(1), catalogReads.Load())

	history, err := st.GetOrCreateSource("beeper", "labels-history")
	setup.NoError(err)
	historyOnly := true
	_, err = st.UpdateSourceSettingsContext(t.Context(), history.ID, store.SourceSettingsUpdate{HistoryOnly: &historyOnly})
	setup.NoError(err)
	retired, err := st.GetOrCreateSource("beeper", "labels-retired")
	setup.NoError(err)
	destination, err := st.GetOrCreateSource("beeper", "labels-destination")
	setup.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired.ID, IntoSourceID: destination.ID})
	setup.NoError(err)
	excluded, err := st.GetOrCreateSource("beeper", "labels-excluded")
	setup.NoError(err)
	for _, sourceID := range []int64{history.ID, retired.ID, excluded.ID} {
		t.Run(strconv.FormatInt(sourceID, 10), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			reads := catalogReads.Load()
			var output map[string]any
			require.Error(run(&output, "labels", "--source-id", strconv.FormatInt(sourceID, 10)))
			assert.Equal(reads, catalogReads.Load(), "ineligible source must refuse before the catalog GET")
			assert.Nil(output)
		})
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			mode.Store(int64(index))
			var out struct {
				Conversations []struct {
					store.ConversationGTD

					Freshness string `json:"freshness"`
					Native    *struct {
						Labels []beeper.Label `json:"labels"`
					} `json:"native"`
				} `json:"conversations"`
			}
			require.NoError(run(&out, "triage", "--source-id", strconv.FormatInt(source.ID, 10), "--refresh"))
			require.Len(out.Conversations, 1)
			row := out.Conversations[0]
			assert.Equal(before, row.ConversationGTD, "native enrichment must preserve durable local state")
			after, err := st.GetConversationGTDContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(before, after)
			if tc.oversized {
				assert.Equal("unavailable", row.Freshness)
				assert.Nil(row.Native, "oversized native observations must be omitted")
				return
			}
			require.NotNil(row.Native)
			assert.Equal("unreviewed", row.Freshness)
			labels := row.Native.Labels
			if tc.unknown {
				assert.Nil(labels, "missing native evidence must remain unknown, not an observed empty list")
			} else {
				assert.NotNil(labels)
				assert.Len(labels, tc.count)
				if tc.count > 0 {
					assert.Equal("label-1", labels[0].ID)
				}
			}
		})
	}
}
