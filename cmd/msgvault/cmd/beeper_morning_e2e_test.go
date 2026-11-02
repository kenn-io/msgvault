package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type morningChat struct {
	id, title     string
	account       string
	messages      []map[string]any
	activity      time.Time
	archived      bool
	draft         any
	archiveStatus int
}
type morningProvider struct {
	t                *testing.T
	mu               sync.Mutex
	chats            map[string]*morningChat
	archives, drafts int
	catalogStalled   bool
	catalogPageSize  int
	catalogSkipPage  bool
	blockFirstSync   bool
	syncInFlight     bool
	blockedSyncReads int
	chatRequests     map[string]int
	pageDelay        time.Duration
	delayChat        string
	editAfterDraft   string
	chatUnavailable  bool
	chatReads        map[string]int
	requestPhases    map[string]morningRequestPhase
	phaseStarted     time.Time
	messageReads     []morningMessageRead
	readsTruncated   bool
}

type morningRequestPhase struct {
	Count        int   `json:"count"`
	Microseconds int64 `json:"microseconds"`
}

type morningMessageRead struct {
	Direction string `json:"direction"`
	Items     int    `json:"items"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// Observe the real subprocess seam without replacing its command or output.
// Command resolution precedes Start; output proves Start succeeded; returning
// from the production runner proves its pipe readers and Wait have returned.
type morningSyncProbe struct {
	*storeAPIAdapter

	provider                *morningProvider
	failAnswerGroups        atomic.Bool
	classificationTarget    atomic.Int64
	classificationFault     atomic.Int32
	classificationCalls     atomic.Int32
	classificationCommitted atomic.Int32
	mu                      sync.Mutex
	started                 time.Time
	phase                   string
	resolved                bool
	output                  bool
	summary                 bool
	returned                bool
}

func (p *morningSyncProbe) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	// Fault delivery only at the existing real adapter boundary; all committed
	// classification and owner changes still pass through the actual Store.
	if p.classificationTarget.Load() > 0 && len(req.Args) > 1 && req.Args[0] == "beeper" && req.Args[1] == "classify" {
		intent, parseErr := parseBeeperArgs(req.Args)
		if parseErr != nil {
			return parseErr
		}
		if intent.ConversationID == p.classificationTarget.Load() {
			p.classificationCalls.Add(1)
			switch p.classificationFault.Swap(0) {
			case 1:
				return p.storeAPIAdapter.RunCLICommand(ctx, req, func(event api.CLIRunEvent) error {
					if event.Type == "stdout" {
						p.classificationCommitted.Add(1)
						return errors.New("synthetic classification response unavailable after commit")
					}
					return emit(event)
				})
			case 2:
				return errors.New("synthetic classification refused before commit")
			case 3:
				current, readErr := p.store.GetConversationGTDContext(ctx, intent.ConversationID)
				if readErr != nil {
					return readErr
				}
				answer := true
				for _, category := range []string{"todo", "waiting"} {
					current, readErr = p.store.SetConversationGTDContext(ctx, intent.ConversationID, store.ConversationGTDChange{
						Category: category, NeedsAnswer: &answer, ExpectedRevision: &current.Revision,
						ObservedActivity: intent.Observed, ContextStamp: intent.Stamp, ReviewedHead: []byte(intent.Head),
					})
					if readErr != nil {
						return readErr
					}
				}
				return errors.New("synthetic concurrent owner classification superseded request")
			}
		}
	}
	if p.failAnswerGroups.Load() && len(req.Args) > 1 && req.Args[0] == "beeper" && req.Args[1] == "triage" {
		for _, arg := range req.Args {
			if strings.HasPrefix(arg, "--needs-answer") {
				return errors.New("synthetic optional group read unavailable")
			}
		}
	}
	if len(req.Args) == 0 || req.Args[0] != "sync-beeper" {
		return p.storeAPIAdapter.RunCLICommand(ctx, req, emit)
	}
	if p.provider != nil {
		first := false
		for i, arg := range req.Args {
			if arg == "--account=signal" || (arg == "--account" && i+1 < len(req.Args) && req.Args[i+1] == "signal") {
				first = true
			}
		}
		p.provider.mu.Lock()
		p.provider.syncInFlight = first
		p.provider.mu.Unlock()
		defer func() { p.provider.mu.Lock(); p.provider.syncInFlight = false; p.provider.mu.Unlock() }()
	}
	p.mu.Lock()
	p.started, p.phase = time.Now(), "runner_entered"
	p.mu.Unlock()
	err := p.storeAPIAdapter.RunCLICommand(ctx, req, func(event api.CLIRunEvent) error {
		p.mu.Lock()
		p.output, p.phase = true, "child_output_observed"
		if event.Type == "stdout" && strings.Contains(event.Data, " done in ") {
			p.summary = true
		}
		p.mu.Unlock()
		return emit(event)
	})
	p.mu.Lock()
	p.returned, p.phase = true, "runner_returned"
	p.mu.Unlock()
	return err
}

func (f *morningProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockFirstSync && f.syncInFlight {
		f.blockedSyncReads++
		f.syncInFlight = false // Only the actual first sync request blocks; local reads remain independent.
		f.mu.Unlock()
		<-r.Context().Done()
		f.mu.Lock()
		return
	}
	phase := "other"
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/accounts"):
		phase = "accounts"
	case r.URL.Path == "/v1/chats/search":
		phase = "catalog"
	case strings.Contains(r.URL.Path, "/messages"):
		phase = "messages"
	}
	started := time.Now()
	messageDirection, messageItems := "", 0
	defer func() {
		if f.requestPhases == nil {
			f.requestPhases = make(map[string]morningRequestPhase)
		}
		stat := f.requestPhases[phase]
		stat.Count++
		stat.Microseconds += time.Since(started).Microseconds()
		f.requestPhases[phase] = stat
		if messageDirection != "" && !f.phaseStarted.IsZero() {
			if len(f.messageReads) == 64 {
				f.messageReads = f.messageReads[1:]
				f.readsTruncated = true
			}
			f.messageReads = append(f.messageReads, morningMessageRead{Direction: messageDirection, Items: messageItems, ElapsedMS: started.Sub(f.phaseStarted).Milliseconds()})
		}
	}()
	write := func(value any) {
		body, err := json.Marshal(value)
		if err != nil {
			assert.Fail(f.t, "fixture JSON encoding failed", "%v", err)
			http.Error(w, "fixture JSON encoding failed", http.StatusInternalServerError)
			return
		}
		_, err = w.Write(body)
		assert.NoError(f.t, err)
	}
	if r.Header.Get("Authorization") != "Bearer synthetic-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/accounts") {
		account := strings.TrimPrefix(r.URL.Path, "/v1/accounts/")
		if r.URL.Path == "/v1/accounts" {
			account = "signal"
		}
		a := map[string]any{"accountID": account, "network": "Signal", "status": "connected", "bridge": map[string]any{"provider": "signal"}, "user": map[string]any{"id": "@self:example.test", "isSelf": true}}
		if r.URL.Path == "/v1/accounts" {
			write([]any{a})
		} else {
			write(a)
		}
		return
	}
	if r.URL.Path == "/v1/labels" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	chatJSON := func(ch *morningChat) map[string]any {
		account := ch.account
		if account == "" {
			account = "signal"
		}
		return map[string]any{"id": ch.id, "accountID": account, "network": "Signal", "title": ch.title, "type": "single", "lastActivity": ch.activity.Format(time.RFC3339Nano), "isArchived": ch.archived, "capabilities": map[string]bool{"archive": true}, "draft": ch.draft, "participants": map[string]any{"items": []any{}, "hasMore": false, "total": 0}}
	}
	if r.URL.Path == "/v1/chats/search" {
		var ids []string
		for id, chat := range f.chats {
			account := chat.account
			if account == "" {
				account = "signal"
			}
			if requested := r.URL.Query().Get("accountIDs"); requested != "" && requested != account {
				continue
			}
			ids = append(ids, id)
		}
		sort.Strings(ids)
		items := []any{}
		for _, id := range ids {
			items = append(items, chatJSON(f.chats[id]))
		}
		if f.catalogStalled {
			write(map[string]any{"items": items, "hasMore": true, "oldestCursor": "stalled-catalog"})
		} else if f.catalogPageSize > 0 {
			start := 0
			if cursor := r.URL.Query().Get("cursor"); cursor != "" {
				var err error
				start, err = strconv.Atoi(strings.TrimPrefix(cursor, "page-"))
				if err != nil || start < 0 || start >= len(items) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			end := min(start+f.catalogPageSize, len(items))
			selected := items[start:end]
			if f.catalogSkipPage && start == 2 {
				for _, raw := range selected {
					entry, ok := raw.(map[string]any)
					if !ok {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					delete(entry, "accountID")
				}
			}
			next := ""
			if end < len(items) {
				next = "page-" + strconv.Itoa(end)
			}
			write(map[string]any{"items": selected, "hasMore": end < len(items), "oldestCursor": next})
		} else {
			write(map[string]any{"items": items, "hasMore": false})
		}
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/chats/")
	parts := strings.Split(path, "/")
	ch := f.chats[parts[0]]
	if ch == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if f.chatRequests == nil {
		f.chatRequests = map[string]int{}
	}
	f.chatRequests[ch.id]++
	if f.chatUnavailable && ch.id == "room4" && len(parts) == 1 && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if ch.id == f.delayChat && len(parts) == 2 && parts[1] == "messages" && r.URL.Query().Get("cursor") == "" && f.pageDelay > 0 {
		delay := f.pageDelay
		f.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-r.Context().Done():
		}
		timer.Stop()
		f.mu.Lock()
		if r.Context().Err() != nil {
			return
		}
	}
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			if f.chatReads == nil {
				f.chatReads = map[string]int{}
			}
			f.chatReads[ch.id]++
		}
		if r.Method == http.MethodPatch {
			var body struct {
				Draft any `json:"draft"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ch.draft != nil {
				w.WriteHeader(http.StatusConflict)
				return
			}
			ch.draft = body.Draft
			if ch.id == f.editAfterDraft && len(ch.messages) > 0 {
				ch.messages[0]["text"] = "Edited after draft creation at unchanged activity"
			}
			f.drafts++
		}
		write(chatJSON(ch))
		return
	}
	if parts[1] == "archive" {
		f.archives++
		if ch.archiveStatus != 0 {
			w.WriteHeader(ch.archiveStatus)
			return
		}
		ch.archived = true
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if parts[1] != "messages" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(parts) == 3 {
		messageDirection = "lookup"
		for _, m := range ch.messages {
			if m["id"] == parts[2] {
				messageItems = 1
				write(m)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	direction := r.URL.Query().Get("direction")
	messageDirection = "unknown"
	switch direction {
	case "":
		messageDirection = "head"
	case "before", "after":
		messageDirection = direction
	}
	// Exclusive opaque cursors. hasMore deliberately lies even on empty head.
	var begin int
	end := len(ch.messages)
	if cursor != "" {
		at := -1
		for i, m := range ch.messages {
			if m["id"] == cursor {
				at = i
				break
			}
		}
		if at < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if direction == "after" {
			begin = at + 1
			end = min(begin+20, end)
		} else {
			end = at
			begin = max(0, end-20)
		}
	} else {
		begin = max(0, end-20)
	}
	items := []any{}
	for i := end - 1; i >= begin; i-- {
		items = append(items, ch.messages[i])
	}
	messageItems = len(items)
	out := map[string]any{"items": items, "hasMore": true, "oldestCursor": nil, "newestCursor": nil}
	if begin < end {
		out["oldestCursor"] = ch.messages[begin]["id"]
		out["newestCursor"] = ch.messages[end-1]["id"]
	}
	write(out)
}

func (f *morningProvider) add(id, title, text string, base time.Time, count int) {
	ch := &morningChat{id: id, title: title, activity: base}
	for i := range count {
		ch.messages = append(ch.messages, map[string]any{"id": fmt.Sprintf("%s-%d", id, i), "chatID": id, "accountID": "signal", "senderID": "@synthetic:example.test", "timestamp": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano), "sortKey": strconv.Itoa(i), "type": "TEXT", "text": text, "isSender": false})
	}
	f.chats[id] = ch
}

// Only the external classifier is substituted. This pins its stable JSONL /
// executable contract without calling a model; all msgvault CLI, HTTP, Store,
// live pagination, draft and archive behavior is production code.
func TestBeeperMorningBuiltCLI(t *testing.T) {
	runBeeperMorningBuiltCLI(t)
}

func runBeeperMorningBuiltCLI(t *testing.T, reviewScenario ...string) {
	t.Helper()
	// Fault cases need only the existing draft chat and their target. The
	// discovery, catalog and preview cases retain the complete native cohort.
	minimal := len(reviewScenario) > 0 && reviewScenario[0] != "catalog"
	binary := os.Getenv("MSGVAULT_E2E_BINARY")
	if binary == "" {
		t.Skip("set MSGVAULT_E2E_BINARY to the make build binary")
	}
	// Finish our own work before go test's hard timeout can skip all cleanup.
	deadline := time.Now().Add(3*time.Minute + 30*time.Second)
	if outer, ok := t.Deadline(); ok && outer.Add(-time.Minute).Before(deadline) {
		deadline = outer.Add(-time.Minute)
	}
	fixtureCtx, cancelFixture := context.WithDeadline(t.Context(), deadline)
	defer cancelFixture()
	binary, err := filepath.Abs(binary)
	require.NoError(t, err)
	root := t.TempDir()
	dbPath := filepath.Join(root, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSourceContext(fixtureCtx, "beeper", "signal")
	require.NoError(t, err)
	provider := &morningProvider{t: t, chats: map[string]*morningChat{}}
	base := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	ids := make([]int64, 8)
	for i, title := range []string{"held-one", "held-two", "old-todo", "draft-todo", "late-todo", "waiting", "delegated", "reference"} {
		chat := fmt.Sprintf("room%d", i)
		count := 1
		if i < 2 {
			count = 0
		}
		if i == 2 {
			count = 65
		}
		provider.add(chat, title, title+" fresh overnight context", base, count)
		if i == 4 || (minimal && i != 3) {
			continue
		} // Genuinely unimported overnight chat: only scoped sync can discover its identity.
		id, err := st.EnsureConversationWithTypeContext(fixtureCtx, source.ID, chat, "direct_chat", title)
		require.NoError(t, err)
		ids[i] = id
	}
	native := httptest.NewServer(provider)
	t.Cleanup(func() {
		native.CloseClientConnections()
		native.Close()
	})
	cfg := &config.Config{HomeDir: root, Data: config.DataConfig{DataDir: root}}
	cfg.Beeper.URL = native.URL
	cfg.Beeper.RateLimitQPS = 1000
	require.NoError(t, beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), beeperDraftPolicy: []config.GmailDraftSource{{SourceID: source.ID, Enabled: true}}}
	// Existing managed response must survive both initial automation and reruns.
	require.NoError(t, adapter.runCLIComposeDraft(fixtureCtx, api.CLIRunRequest{Args: []string{"draft-compose", "--source-id", strconv.FormatInt(source.ID, 10), "--to", "room3", "--body", "Existing response", "--json"}}, func(api.CLIRunEvent) error { return nil }))
	existing, err := st.LiveBeeperDraftContext(fixtureCtx, source.ID, "room3")
	require.NoError(t, err)
	if !minimal {
		old, err := adapter.runBeeperContextSnapshot(fixtureCtx, ids[2], beeper.DefaultContextBounds(), nil, false)
		require.NoError(t, err)
		require.True(t, old.Complete, old.Reason)
		assert.True(t, old.OlderHistoryOmitted)
		zero := int64(0)
		_, err = st.SetConversationGTDContext(fixtureCtx, ids[2], store.ConversationGTDChange{Category: "todo", ExpectedRevision: &zero, ObservedActivity: old.Activity, ContextStamp: old.Stamp, ReviewedHead: mustJSON(t, old.Head)})
		require.NoError(t, err)
	}
	provider.mu.Lock()
	provider.chats["room2"].messages = append(provider.chats["room2"].messages, map[string]any{"id": "room2-new", "chatID": "room2", "accountID": "signal", "senderID": "@synthetic:example.test", "timestamp": base.Add(3 * time.Hour).Format(time.RFC3339Nano), "type": "TEXT", "text": "NEW message on old-todo"})
	provider.chats["room2"].activity = base.Add(3 * time.Hour)
	provider.mu.Unlock()
	// Real subprocess sync runs the actual built binary, not the go test process.
	previousResolver := daemonCLIExecutableResolver
	probe := &morningSyncProbe{storeAPIAdapter: adapter, provider: provider, phase: "not_invoked"}
	daemonCLIExecutableResolver = func() (string, error) {
		probe.mu.Lock()
		probe.resolved = true
		probe.mu.Unlock()
		return binary, nil
	}
	t.Cleanup(func() { daemonCLIExecutableResolver = previousResolver })
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: probe, Engine: query.NewSQLiteEngine(st.DB()), Logger: adapter.logger})
	remote := httptest.NewServer(server.Router())
	t.Cleanup(func() {
		remote.CloseClientConnections()
		remote.Close()
	})
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, server.Shutdown(cleanupCtx))
	})
	home := filepath.Join(root, "cli-home")
	require.NoError(t, os.MkdirAll(home, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir=%q\n[beeper]\nurl=%q\nrate_limit_qps=1000\n[telemetry]\nenabled=false\n", root, native.URL)), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[remote]\nurl=%q\nallow_insecure=true\n[telemetry]\nenabled=false\n", remote.URL)), 0600))
	// The adapter invocation supplies the daemon host config to the REAL child.
	adapter.options = invocationOptions{homeDir: root, homeDirChanged: true}
	classifier := filepath.Join(root, "pi-fixture")
	require.NoError(t, os.WriteFile(classifier, []byte(morningClassifierFixture), 0700))
	script, err := filepath.Abs("../../../scripts/beeper-morning.py")
	require.NoError(t, err)
	runnerConfig := map[string]any{"enabled": true, "apply": true, "prepare_drafts": true, "archive_policy": "archive-selected", "msgvault_binary": binary, "msgvault_home": home, "pi_binary": classifier, "provider": "zai", "model": "fixture", "model_auth_env": "ZAI_API_KEY", "state_dir": filepath.Join(root, "state"), "source_ids": []int64{source.ID}, "page_size": 2, "max_chats": 2, "sync_sources": true,
		"run_timeout": 35, "process_timeout": 15, "context_timeout": 5}
	runnerTemp := filepath.Join(root, "runner-tmp")
	require.NoError(t, os.Mkdir(runnerTemp, 0700))
	configPath := filepath.Join(root, "runner.json")
	require.NoError(t, os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
	syncDiagnostics := func() string {
		// Project only fixed phases and counters, never provider identities,
		// raw child output, sync error text, cursors, auth or content.
		probe.mu.Lock()
		elapsed := int64(0)
		if !probe.started.IsZero() {
			elapsed = time.Since(probe.started).Milliseconds()
		}
		data := map[string]any{"child_phase": probe.phase, "command_resolved": probe.resolved, "child_output_observed": probe.output, "import_summary_observed": probe.summary, "runner_returned": probe.returned, "elapsed_ms": elapsed}
		probe.mu.Unlock()
		provider.mu.Lock()
		phases := map[string]morningRequestPhase{}
		for _, phase := range []string{"accounts", "catalog", "messages", "other"} {
			phases[phase] = provider.requestPhases[phase]
		}
		data["provider_phases"] = phases
		data["message_reads"] = append([]morningMessageRead(nil), provider.messageReads...)
		data["message_reads_truncated"] = provider.readsTruncated
		provider.mu.Unlock()
		readCtx, cancel := context.WithTimeout(fixtureCtx, time.Second)
		defer cancel()
		run, readErr := st.GetLatestSyncContext(readCtx, source.ID, 0)
		data["sync_status"] = "unavailable"
		if errors.Is(readErr, store.ErrSyncRunNotFound) {
			data["sync_status"] = "none"
		} else if readErr == nil {
			switch run.Status {
			case store.SyncStatusRunning, store.SyncStatusCompleted, store.SyncStatusFailed, store.SyncStatusCancelled, store.SyncStatusPaused:
				data["sync_status"] = run.Status
			}
			data["messages_processed"], data["messages_added"], data["sync_errors"] = run.MessagesProcessed, run.MessagesAdded, run.ErrorsCount
			data["sync_error_class"] = "none"
			if run.ErrorMessage.Valid {
				text := strings.ToLower(run.ErrorMessage.String)
				kind := "other"
				switch {
				case strings.Contains(text, "database is locked"), strings.Contains(text, "database table is locked"), strings.Contains(text, "database busy"):
					kind = "database_busy"
				case strings.Contains(text, context.DeadlineExceeded.Error()):
					kind = "deadline_exceeded"
				case strings.Contains(text, context.Canceled.Error()):
					kind = "context_canceled"
				case strings.Contains(text, store.ErrSyncRunSuperseded.Error()):
					kind = "sync_superseded"
				}
				data["sync_error_class"] = kind
			}
		}
		data["actual_committed_messages"] = "unavailable"
		if count, countErr := st.CountMessagesForSourceContext(readCtx, source.ID); countErr == nil {
			data["actual_committed_messages"] = count
		}
		return string(mustJSON(t, data))
	}
	invoke := func() (map[string]any, error) {
		t.Helper()
		callCtx, cancel := context.WithTimeout(fixtureCtx, 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(callCtx, "python3", script, "--config", configPath)
		// Python's default SIGINT handler unwinds execute's finally (killpg +
		// wait) and TemporaryDirectory; CommandContext's default SIGKILL cannot.
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 10 * time.Second
		cmd.Env = []string{"PATH=/usr/bin:/bin", "ZAI_API_KEY=synthetic-classifier-key", "MSGVAULT_API_KEY=must-not-leak", "BEEPER_ACCESS_TOKEN=must-not-leak", "NODE_OPTIONS=must-not-leak", "HOME=" + root, "TMPDIR=" + runnerTemp}
		var out, errs bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errs
		err := cmd.Run()
		if err != nil {
			return nil, fmt.Errorf("%w: %s; fixture_sync=%s", err, errs.String(), syncDiagnostics())
		}
		var report map[string]any
		err = json.Unmarshal(out.Bytes(), &report)
		return report, err
	}
	if !minimal {
		// Preview exercises the real runner and owner commands but must not sync a
		// new identity, classify, prepare a draft, or archive even with sync opted in.
		beforePreview, err := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, err)
		provider.mu.Lock()
		previewPosts, previewDrafts := provider.archives, provider.drafts
		provider.mu.Unlock()
		runnerConfig["apply"] = false
		runnerConfig["page_size"] = 200
		runnerConfig["max_chats"] = 200
		require.NoError(t, os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
		preview, err := invoke()
		require.NoError(t, err)
		assert.Equal(t, "preview", preview["status"])
		previewCoverage, ok := preview["provider_coverage"].([]any)
		require.True(t, ok, "coverage must be an array")
		require.Len(t, previewCoverage, 1)
		previewSource, ok := previewCoverage[0].(map[string]any)
		require.True(t, ok, "coverage entry must be an object")
		unknownIdentities, ok := previewSource["unknown_local_identities"].(float64)
		require.True(t, ok, "unknown local identity count must be numeric")
		assert.Equal(t, "1", strconv.FormatFloat(unknownIdentities, 'f', -1, 64), "the new provider chat must remain genuinely unimported in preview; count must be the exact integer")
		afterPreview, err := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, err)
		assert.Equal(t, beforePreview, afterPreview, "preview must not import or classify the new overnight chat")
		provider.mu.Lock()
		assert.Equal(t, previewPosts, provider.archives)
		assert.Equal(t, previewDrafts, provider.drafts)
		provider.mu.Unlock()
		if len(reviewScenario) == 0 {
			previewResults, ok := preview["results"].([]any)
			require.True(t, ok)
			assert.Contains(t, previewResults, map[string]any{"source_id": float64(source.ID), "status": "sync_skipped_preview"})
		}
	}
	runnerConfig["apply"] = true
	runnerConfig["page_size"] = 2
	runnerConfig["max_chats"] = 2
	require.NoError(t, os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
	provider.mu.Lock()
	provider.requestPhases = nil
	provider.phaseStarted = time.Now()
	provider.messageReads, provider.readsTruncated = nil, false
	provider.mu.Unlock()
	// Check page-granular progress after the first two actual discovery passes
	// in the complete CI workflow; no extra invokes.
	var firstOldest, firstNewest string
	checkDiscoveryPass := func(pass int) {
		t.Helper()
		run, err := st.GetLatestSyncContext(fixtureCtx, source.ID, 0)
		require.NoError(t, err)
		require.Equal(t, store.SyncStatusCompleted, run.Status)
		state, err := beeper.LoadSyncState(run.CursorAfter.String)
		require.NoError(t, err)
		require.Contains(t, state.Chats, "room2")
		current := state.Chats["room2"]
		if pass == 0 {
			rows, err := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
			require.NoError(t, err)
			assert.Len(t, rows, 8, "the real scoped sync must discover the unimported overnight identity")
			assert.False(t, current.Done, "discovery must leave long history resumable while visiting later chats")
			assert.NotEmpty(t, current.Oldest)
			assert.NotEmpty(t, current.Newest)
			firstOldest, firstNewest = current.Oldest, current.Newest
		} else {
			assert.False(t, current.Done, "two pages still omit older history in this fixture")
			assert.NotEqual(t, firstOldest, current.Oldest, "the next discovery pass must advance the durable backfill cursor")
			assert.Equal(t, firstNewest, current.Newest)
		}
		count, err := st.CountMessagesForSourceContext(fixtureCtx, source.ID)
		require.NoError(t, err)
		if pass == 0 {
			assert.EqualValues(t, 25, count, "one provider page of long history plus five later single-event chats")
		} else {
			assert.EqualValues(t, 45, count, "the next pass must archive the next page without duplicating existing messages")
		}
	}
	var late store.ConversationGTD
	var rows []store.ConversationGTD
	var postCount, draftCount int
	if len(reviewScenario) > 0 {
		// Focused fault cases begin from already reviewed archive identities.
		// The full morning case below retains real discovery, all categories,
		// changed old chats, seven initial passes and five unchanged restarts.
		priorRows, readErr := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, readErr)
		for _, prior := range priorRows {
			packet, captureErr := adapter.runBeeperContextSnapshot(fixtureCtx, prior.ConversationID, beeper.DefaultContextBounds(), nil, true)
			require.NoError(t, captureErr)
			if !packet.Complete {
				continue
			}
			category := prior.Category
			if category == "" {
				category = "reference"
			}
			_, saveErr := st.SetConversationGTDContext(fixtureCtx, prior.ConversationID, store.ConversationGTDChange{
				Category: category, ExpectedRevision: &prior.Revision, ObservedActivity: packet.Activity,
				ContextStamp: packet.Stamp, ReviewedHead: mustJSON(t, packet.Head),
			})
			require.NoError(t, saveErr)
		}
		ident, createErr := st.EnsureConversation(source.ID, "room4", "reference-new")
		require.NoError(t, createErr)
		late, err = st.GetConversationGTDContext(fixtureCtx, ident)
		require.NoError(t, err)
	} else {
		for pass := range 7 {
			report, err := invoke()
			// Fixed diagnostic fields only: no message, draft, token or raw child output.
			targets, readErr := st.BeeperConversationIDsContext(fixtureCtx, source.ID, []string{"room4"})
			require.NoError(t, readErr)
			diagnostic := map[string]any{"pass": pass + 1, "target_present": targets["room4"] > 0, "scanned": report["scanned"], "status": report["status"], "results": report["results"]}
			progressData, readErr := os.ReadFile(filepath.Join(root, "state", "progress.json"))
			if readErr == nil {
				var progress map[string]any
				if json.Unmarshal(progressData, &progress) == nil {
					diagnostic["progress"] = progress["sources"]
				}
			}
			t.Logf("setup_scan=%s", mustJSON(t, diagnostic))
			require.NoError(t, err)
			if pass < 2 {
				checkDiscoveryPass(pass)
			}
		}
		rows, err = st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, err)
		classes := map[string]bool{}
		for _, row := range rows {
			if row.Category != "" {
				classes[row.Category] = true
			}
			if row.ChatID == "room4" {
				late = row
			}
		}
		assert.Equal(t, map[string]bool{"todo": true, "waiting": true, "delegated": true, "reference": true}, classes)
		require.NotZero(t, late.Revision, "scoped sync must import the unarchived late chat and fair scan must reach it")
		changed, err := st.GetConversationGTDContext(fixtureCtx, ids[2])
		require.NoError(t, err)
		assert.Greater(t, changed.Revision, int64(1))
		reused, err := st.LiveBeeperDraftContext(fixtureCtx, source.ID, "room3")
		require.NoError(t, err)
		assert.Equal(t, existing.DraftID, reused.DraftID)
		assert.Equal(t, existing.Text, reused.Text)
		created, err := st.LiveBeeperDraftContext(fixtureCtx, source.ID, "room2")
		require.NoError(t, err)
		assert.NotNil(t, created.Text)
		provider.mu.Lock()
		postCount, draftCount = provider.archives, provider.drafts
		provider.mu.Unlock()
		revisions := map[int64]int64{}
		for _, row := range rows {
			revisions[row.ConversationID] = row.Revision
		}
		for range 5 {
			_, err := invoke()
			require.NoError(t, err)
		}
		provider.mu.Lock()
		assert.Equal(t, postCount, provider.archives)
		assert.Equal(t, draftCount, provider.drafts)
		assert.True(t, provider.chats["room4"].archived)
		provider.mu.Unlock()
		rows, err = st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, err)
		for _, row := range rows {
			assert.Equal(t, revisions[row.ConversationID], row.Revision)
		}
		reopened, err := store.OpenForTest(dbPath)
		require.NoError(t, err)
		defer func() { assert.NoError(t, reopened.Close()) }()
		answers, err := reopened.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, true)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(answers), 3)
	}

	configureCase := func(mode string) {
		t.Helper()
		runnerConfig["sync_sources"] = false
		runnerConfig["page_size"] = 200
		runnerConfig["max_chats"] = 200
		runnerConfig["state_dir"] = filepath.Join(root, "state-"+mode)
		runnerConfig["max_input_bytes"] = 2 << 20
		require.NoError(t, os.WriteFile(filepath.Join(root, "fixture-mode"), []byte(mode), 0600))
		require.NoError(t, os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
	}
	resetLate := func() store.ConversationGTD {
		t.Helper()
		state, err := st.SetConversationGTDContext(fixtureCtx, late.ConversationID, store.ConversationGTDChange{Clear: true})
		require.NoError(t, err)
		provider.mu.Lock()
		provider.chats["room4"].archived = false
		provider.mu.Unlock()
		return state
	}
	runReviewCases := func(t *testing.T, scenario string) {
		t.Helper()
		runnerConfig["source_ids"] = []int64{source.ID}
		switch scenario {
		case "classification-ack":
			for _, mode := range []string{"committed-response-lost", "not-committed", "owner-superseded"} {
				t.Run(mode, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					before := resetLate()
					configureCase("classification-" + mode)
					// Keep the real fixture classifier's requested-draft decision.
					require.NoError(os.WriteFile(filepath.Join(root, "fixture-mode"), []byte("request-draft"), 0600))
					probe.classificationTarget.Store(late.ConversationID)
					probe.classificationCommitted.Store(0)
					probe.classificationFault.Store(1)
					switch mode {
					case "not-committed":
						probe.classificationFault.Store(2)
					case "owner-superseded":
						probe.classificationFault.Store(3)
					}
					t.Cleanup(func() { probe.classificationTarget.Store(0); probe.classificationFault.Store(0) })
					calls := probe.classificationCalls.Load()
					provider.mu.Lock()
					beforePosts, beforeDrafts := provider.archives, provider.drafts
					provider.mu.Unlock()
					report, err := invoke()
					require.NoError(err, "a classification delivery failure must be an explicit unresolved outcome")
					assert.Equal(calls+1, probe.classificationCalls.Load())
					current, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
					require.NoError(err)
					switch mode {
					case "committed-response-lost":
						assert.Equal(int32(1), probe.classificationCommitted.Load(), "failure must follow the actual adapter's committed result")
						assert.Equal(before.Revision+1, current.Revision)
						assert.Len(current.ContextStamp, 64)
						assert.True(current.ReviewedHead.IsValid())
					case "not-committed":
						assert.Zero(probe.classificationCommitted.Load())
						assert.Equal(before, current)
					default:
						assert.Zero(probe.classificationCommitted.Load())
						assert.Equal(before.Revision+2, current.Revision)
						assert.Equal("waiting", current.Category)
						assert.True(current.NeedsAnswer)
					}
					provider.mu.Lock()
					assert.Equal(beforePosts, provider.archives, "unknown classification cannot authorize downstream archive")
					assert.Equal(beforeDrafts, provider.drafts, "unknown classification cannot authorize draft preparation")
					provider.mu.Unlock()
					stateDir, ok := runnerConfig["state_dir"].(string)
					require.True(ok)
					checkDiagnostic := func(report map[string]any, want bool) {
						t.Helper()
						unresolved, ok := report["unresolved"].([]any)
						require.True(ok)
						expected := map[string]any{"conversation_id": float64(late.ConversationID), "source_id": float64(source.ID), "revision": float64(before.Revision + 1), "status": "classification_changed_or_unavailable"}
						if want {
							assert.Contains(unresolved, expected, "the possible r+1 commit remains uncertain, never a completion receipt")
						} else {
							for _, raw := range unresolved {
								item, ok := raw.(map[string]any)
								require.True(ok)
								assert.NotEqual(float64(late.ConversationID), item["conversation_id"], "another actual revision supersedes this diagnostic")
							}
						}
					}
					checkDiagnostic(report, mode != "owner-superseded")
					body, err := os.ReadFile(filepath.Join(stateDir, "report.json"))
					require.NoError(err)
					var persisted map[string]any
					require.NoError(json.Unmarshal(body, &persisted))
					checkDiagnostic(persisted, mode != "owner-superseded")
					if mode == "not-committed" {
						// A new full context/model pass may decide again after a definite
						// no-commit; there is no saved reviewed head to skip that pass.
						provider.mu.Lock()
						beforeReads := provider.chatReads["room4"]
						provider.mu.Unlock()
						_, err = invoke()
						require.NoError(err)
						after, readErr := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
						require.NoError(readErr)
						assert.Equal(before.Revision+1, after.Revision)
						assert.Equal(calls+2, probe.classificationCalls.Load())
						provider.mu.Lock()
						assert.Greater(provider.chatReads["room4"], beforeReads, "a new decision must re-read live context")
						assert.Equal(beforePosts+1, provider.archives)
						assert.Equal(beforeDrafts+1, provider.drafts)
						provider.mu.Unlock()
						return
					}
					calls = probe.classificationCalls.Load()
					report, err = invoke()
					require.NoError(err)
					checkDiagnostic(report, mode == "committed-response-lost")
					after, readErr := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
					require.NoError(readErr)
					assert.Equal(current, after, "unchanged restart must preserve the actual reviewed head, user decision and revision")
					assert.Equal(calls, probe.classificationCalls.Load(), "unchanged restart must not replay classification")
					provider.mu.Lock()
					assert.Equal(beforePosts, provider.archives)
					assert.Equal(beforeDrafts, provider.drafts)
					provider.mu.Unlock()
				})
			}
		case "budget":
			for _, withDraft := range []bool{false, true} {
				// This fixture's one-event initial scope is five physical reads.
				// C5+V5+A5+P1+F1+W1+B1=19; new draft GET+PATCH adds2.
				exact := 19
				if withDraft {
					exact += 2
				}
				for _, allowance := range []int{exact - 1, exact} {
					t.Run(fmt.Sprintf("draft-%t/cap-%d", withDraft, allowance), func(t *testing.T) {
						assert := assert.New(t)
						require := require.New(t)
						target := fmt.Sprintf("budget-%t-%d", withDraft, allowance)
						provider.mu.Lock()
						provider.add(target, "reference-budget", "Synthetic bounded request decision", base, 1)
						provider.mu.Unlock()
						_, err := st.EnsureConversation(source.ID, target, "reference-budget")
						require.NoError(err)
						configureCase("request-budget")
						runnerConfig["prepare_drafts"] = withDraft
						runnerConfig["max_context_requests"] = allowance
						require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
						provider.mu.Lock()
						provider.chats[target].draft = nil
						provider.chatRequests = map[string]int{}
						beforePosts := provider.archives
						provider.mu.Unlock()
						_, err = invoke()
						require.NoError(err)
						provider.mu.Lock()
						used := provider.chatRequests[target]
						archived := provider.chats[target].archived
						posts := provider.archives
						provider.mu.Unlock()
						assert.LessOrEqual(used, allowance, "count server-observed requests, including PATCH/POST/readback")
						if allowance == exact {
							assert.Equal(exact, used)
							assert.True(archived)
							assert.Equal(beforePosts+1, posts)
						} else {
							assert.False(archived)
							assert.Equal(beforePosts, posts, "reserve every write/readback request before a POST")
						}
					})
				}
			}
		case "deadline":
			for _, mode := range []string{"inference-delay", "provider-delay"} {
				t.Run(mode, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					target := "deadline-" + mode
					provider.mu.Lock()
					provider.add(target, "reference-deadline", "Synthetic provider deadline decision", base, 1)
					provider.mu.Unlock()
					_, err := st.EnsureConversation(source.ID, target, "reference-deadline")
					require.NoError(err)
					configureCase(mode)
					runnerConfig["prepare_drafts"] = false
					runnerConfig["context_timeout"] = 5
					require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
					provider.mu.Lock()
					provider.pageDelay = 0
					provider.delayChat = target
					if mode == "provider-delay" {
						// Each capture fits5s separately; two head-page delays
						// exceed the cumulative5s without widening outer caps.
						provider.pageDelay = 3 * time.Second
					}
					beforePosts := provider.archives
					provider.mu.Unlock()
					t.Cleanup(func() { provider.mu.Lock(); provider.pageDelay = 0; provider.mu.Unlock() })
					_, err = invoke()
					require.NoError(err)
					provider.mu.Lock()
					if mode == "inference-delay" {
						assert.True(provider.chats[target].archived, "six seconds of inference is outside the active-provider5s budget")
						assert.Equal(beforePosts+1, provider.archives)
					} else {
						assert.False(provider.chats[target].archived)
						assert.Equal(beforePosts, provider.archives, "provider time cannot renew per capture")
					}
					provider.mu.Unlock()
				})
			}
		case "reconciliation":
			for _, mode := range []string{"archived", "false", "unavailable", "local-ack"} {
				t.Run(mode, func(t *testing.T) {
					requirements := require.New(t)
					t.Cleanup(func() {
						probe.failAnswerGroups.Store(false)
						provider.mu.Lock()
						provider.chatUnavailable = false
						provider.mu.Unlock()
					})
					configureCase("reconcile-" + mode)
					runnerConfig["prepare_drafts"] = false
					requirements.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
					seed, err := invoke()
					requirements.NoError(err)
					current, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
					requirements.NoError(err)
					draftRow, err := st.GetConversationGTDContext(fixtureCtx, ids[3])
					requirements.NoError(err)
					for _, status := range []string{"archive_unavailable", "provider_rejected"} {
						t.Run(status, func(t *testing.T) {
							assert := assert.New(t)
							require := require.New(t)
							prior := map[string]any{"conversation_id": late.ConversationID, "source_id": source.ID, "revision": current.Revision, "status": status}
							draftPrior := map[string]any{"conversation_id": ids[3], "source_id": source.ID, "revision": draftRow.Revision, "status": "draft_held"}
							seed["unresolved"] = []any{prior, draftPrior}
							stateDir, ok := runnerConfig["state_dir"].(string)
							require.True(ok)
							reportPath := filepath.Join(stateDir, "report.json")
							require.NoError(os.WriteFile(reportPath, mustJSON(t, seed), 0600))
							held, holdErr := st.GetConversationGTDContext(fixtureCtx, current.ConversationID)
							require.NoError(holdErr)
							require.Equal(current.ConversationID, held.ConversationID)
							require.Equal(current.SourceID, held.SourceID)
							require.Equal(current.ChatID, held.ChatID)
							require.Equal(current.Revision, held.Revision)
							require.Equal(current.Category, held.Category)
							require.Equal(current.NeedsAnswer, held.NeedsAnswer)
							if !held.ArchiveHold {
								require.NoError(st.SetConversationGTDHoldContext(fixtureCtx, current.ConversationID, current.Revision, true))
							}
							provider.mu.Lock()
							provider.chats["room4"].archived = mode == "archived"
							provider.chats["room3"].archived = true
							provider.chatUnavailable = mode == "unavailable"
							beforePosts := provider.archives
							provider.mu.Unlock()
							if mode == "local-ack" {
								var acknowledged bytes.Buffer
								require.NoError(adapter.runCLIBeeper(fixtureCtx, api.CLIRunRequest{Args: []string{"beeper", "archive", "--conversation", strconv.FormatInt(current.ConversationID, 10), "--resolve-hold", "--apply", "--json"}}, func(event api.CLIRunEvent) error {
									if event.Type == "stdout" {
										acknowledged.WriteString(event.Data)
									}
									return nil
								}))
								assert.Contains(acknowledged.String(), "hold_resolved")
							}
							probe.failAnswerGroups.Store(true)
							_, err = invoke()
							require.Error(err, "optional group read remains fallible after reconciliation")
							body, err := os.ReadFile(reportPath)
							require.NoError(err)
							var durable map[string]any
							require.NoError(json.Unmarshal(body, &durable))
							var retained []map[string]any
							encoded, err := json.Marshal(durable["unresolved"])
							require.NoError(err)
							require.NoError(json.Unmarshal(encoded, &retained))
							// Normalize through JSON for exact numeric types.
							var normalized []map[string]any
							require.NoError(json.Unmarshal(mustJSON(t, []any{prior, draftPrior}), &normalized))
							if mode == "archived" {
								assert.NotContains(retained, normalized[0])
							} else {
								assert.Contains(retained, normalized[0])
							}
							assert.Contains(retained, normalized[1], "archival is not proof of resolving draft preparation")
							probe.failAnswerGroups.Store(false)
							after, err := st.GetConversationGTDContext(fixtureCtx, current.ConversationID)
							require.NoError(err)
							assert.Equal(current.Category, after.Category)
							assert.Equal(current.NeedsAnswer, after.NeedsAnswer)
							assert.Equal(current.Revision, after.Revision)
							provider.mu.Lock()
							assert.Equal(beforePosts, provider.archives, "reconciliation and local acknowledgment cannot POST")
							provider.chatUnavailable = false
							provider.mu.Unlock()
						})
					}
				})
			}
		case "draft-after-write":
			assert := assert.New(t)
			require := require.New(t)
			target := "after-draft-reference"
			provider.mu.Lock()
			provider.add(target, "reference-after-draft", "Original reviewed request", base, 1)
			provider.editAfterDraft = target
			beforePosts := provider.archives
			provider.mu.Unlock()
			t.Cleanup(func() { provider.mu.Lock(); provider.editAfterDraft = ""; provider.mu.Unlock() })
			_, err := st.EnsureConversation(source.ID, target, "reference-after-draft")
			require.NoError(err)
			configureCase("request-draft")
			runnerConfig["prepare_drafts"] = true
			require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
			_, err = invoke()
			require.NoError(err)
			provider.mu.Lock()
			assert.Equal(beforePosts, provider.archives)
			assert.False(provider.chats[target].archived)
			assert.NotNil(provider.chats[target].draft, "the prepared draft remains untouched")
			provider.mu.Unlock()
			managed, err := st.LiveBeeperDraftContext(fixtureCtx, source.ID, target)
			require.NoError(err)
			require.NotNil(managed.Text)
			assert.Equal("Prepared synthetic response", *managed.Text)
			assert.Nil(managed.Pending)
		case "draft":
			for _, state := range []string{"removed", "replaced", "pending", "disabled"} {
				t.Run(state, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					managed, err := st.GetBeeperDraftContext(fixtureCtx, existing.DraftID)
					require.NoError(err)
					if state == "pending" {
						managed, err = st.ClaimBeeperDraftContext(fixtureCtx, managed.DraftID, managed.Revision, store.BeeperDraftOperationEdit, "Synthetic pending response")
						require.NoError(err)
					}
					_, err = st.SetConversationGTDContext(fixtureCtx, ids[3], store.ConversationGTDChange{Clear: true})
					require.NoError(err)
					configureCase("request-draft")
					runnerConfig["prepare_drafts"] = state != "disabled"
					require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
					provider.mu.Lock()
					provider.chats["room3"].archived = false
					provider.chats["room3"].draft = nil
					if state != "removed" {
						provider.chats["room3"].draft = map[string]any{"text": "Externally replaced composer"}
					}
					beforePosts, beforeDrafts := provider.archives, provider.drafts
					provider.mu.Unlock()
					_, err = invoke()
					require.NoError(err)
					provider.mu.Lock()
					if state == "disabled" {
						assert.Equal(beforePosts+1, provider.archives)
						assert.True(provider.chats["room3"].archived)
					} else {
						assert.Equal(beforePosts, provider.archives)
						assert.False(provider.chats["room3"].archived)
					}
					assert.Equal(beforeDrafts, provider.drafts, "verification/reuse must never replace a native draft")
					provider.mu.Unlock()
					after, err := st.GetBeeperDraftContext(fixtureCtx, existing.DraftID)
					require.NoError(err)
					assert.Equal(managed.DraftID, after.DraftID)
					assert.Equal(managed.Text, after.Text)
					assert.Equal(managed.Revision, after.Revision)
				})
			}
		case "report":
			for _, status := range []string{"draft_held", "provider_rejected"} {
				t.Run(status, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					resetLate()
					mode := "report-conflict"
					if status == "provider_rejected" {
						mode = "report-rejected"
					}
					configureCase(mode)
					runnerConfig["prepare_drafts"] = status == "draft_held"
					require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
					provider.mu.Lock()
					provider.chats["room4"].draft = map[string]any{"text": "Human composer remains untouched"}
					provider.chats["room4"].archiveStatus = http.StatusForbidden
					beforePosts, beforeDrafts := provider.archives, provider.drafts
					provider.mu.Unlock()
					probe.failAnswerGroups.Store(true)
					t.Cleanup(func() {
						probe.failAnswerGroups.Store(false)
						provider.mu.Lock()
						provider.chats["room4"].archiveStatus = 0
						provider.mu.Unlock()
					})
					_, err := invoke()
					require.Error(err, "the optional final group read is deliberately unavailable")
					current, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
					require.NoError(err)
					assert.Equal("reference", current.Category)
					assert.False(current.NeedsAnswer, "diagnostics cannot rely on inclusion in answer-needed groups")
					provider.mu.Lock()
					expectedPosts := beforePosts
					if status == "provider_rejected" {
						expectedPosts++
					}
					assert.Equal(expectedPosts, provider.archives, "the real conflict/rejection must occur before group failure")
					assert.Equal(beforeDrafts, provider.drafts)
					provider.mu.Unlock()
					stateDir, ok := runnerConfig["state_dir"].(string)
					require.True(ok)
					body, err := os.ReadFile(filepath.Join(stateDir, "report.json"))
					require.NoError(err, "unresolved projection must be durable before optional enrichment")
					var retained map[string]any
					require.NoError(json.Unmarshal(body, &retained))
					assert.NotContains(string(body), "Human composer remains untouched")
					assert.NotContains(string(body), "Requested synthetic response")
					expected := map[string]any{"conversation_id": float64(late.ConversationID), "source_id": float64(source.ID), "revision": float64(current.Revision), "status": status}
					assert.Contains(retained["unresolved"], expected)
					probe.failAnswerGroups.Store(false)
					report, err := invoke()
					require.NoError(err)
					assert.Contains(report["unresolved"], expected)
					after, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
					require.NoError(err)
					assert.Equal(current.Revision, after.Revision, "restart must not replay reviewed classification")
					provider.mu.Lock()
					assert.Equal(expectedPosts, provider.archives)
					assert.Equal(beforeDrafts, provider.drafts)
					provider.mu.Unlock()
				})
			}
		case "sync":
			assert := assert.New(t)
			require := require.New(t)
			second, err := st.GetOrCreateSource("beeper", "synthetic-second")
			require.NoError(err)
			secondChat, err := st.EnsureConversation(second.ID, "second-reference", "reference-second")
			require.NoError(err)
			provider.mu.Lock()
			provider.add("second-reference", "reference-second", "Synthetic second-source reference", base, 1)
			provider.chats["second-reference"].account = "synthetic-second"
			for _, message := range provider.chats["second-reference"].messages {
				message["accountID"] = "synthetic-second"
			}
			provider.blockFirstSync = true
			beforeReads := provider.chatReads["room4"]
			provider.mu.Unlock()
			t.Cleanup(func() { provider.mu.Lock(); provider.blockFirstSync = false; provider.mu.Unlock() })
			resetLate()
			configureCase("sync-fairness")
			runnerConfig["source_ids"] = []int64{source.ID, second.ID}
			runnerConfig["sync_sources"] = true
			runnerConfig["prepare_drafts"] = false
			require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
			for range 2 {
				provider.mu.Lock()
				beforeInvocationReads := provider.chatReads["room4"]
				provider.mu.Unlock()
				_, err = invoke()
				require.NoError(err, "optional first-source child timeout must not abort independent local/later-source work")
				provider.mu.Lock()
				assert.Positive(provider.blockedSyncReads)
				assert.Greater(provider.chatReads["room4"], beforeInvocationReads, "local live context must be reached within this invocation")
				provider.mu.Unlock()
				reached, readErr := st.GetConversationGTDContext(fixtureCtx, secondChat)
				require.NoError(readErr)
				assert.Equal("reference", reached.Category, "later source must be reached before this invocation returns")
				assert.Positive(reached.Revision)
			}
			provider.mu.Lock()
			assert.Positive(provider.blockedSyncReads, "the actual sync child must reach a blocked native request")
			assert.Greater(provider.chatReads["room4"], beforeReads, "first-source live context must still be attempted")
			provider.mu.Unlock()
			after, err := st.GetConversationGTDContext(fixtureCtx, secondChat)
			require.NoError(err)
			assert.Equal("reference", after.Category, "later source must not be starved by optional sync timeout")
			assert.Positive(after.Revision)
		case "catalog":
			assert := assert.New(t)
			require := require.New(t)
			provider.mu.Lock()
			provider.add("zz-native-reference", "reference-native-late", "Synthetic later native reference", base.Add(6*time.Hour), 1)
			provider.catalogPageSize, provider.catalogSkipPage = 2, true
			provider.mu.Unlock()
			t.Cleanup(func() {
				provider.mu.Lock()
				provider.catalogPageSize, provider.catalogSkipPage = 0, false
				provider.mu.Unlock()
			})
			configureCase("catalog-pagination")
			runnerConfig["catalog_pages"] = 1
			runnerConfig["max_chats"], runnerConfig["page_size"] = 2, 2
			runnerConfig["prepare_drafts"] = false
			require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
			previousRevision := int64(0)
			previousRequests, previousChatReads := 0, 0
			catalogDiagnostic := func(stage string, pass int, report map[string]any, commandFailed bool) map[string]any {
				t.Helper()
				data := map[string]any{"stage": stage, "pass": pass, "command_failed": commandFailed, "target_present": false, "target_result_present": false, "target_status": "not_observed", "classification_attempt_observed": false}
				if scanned, ok := report["scanned"].(float64); ok {
					data["scanned"] = scanned
				}
				targetIDs, identityErr := st.BeeperConversationIDsContext(fixtureCtx, source.ID, []string{"zz-native-reference"})
				targetID := targetIDs["zz-native-reference"]
				data["identity_metadata_available"] = identityErr == nil
				data["target_present"] = identityErr == nil && targetID > 0
				var revision int64
				if targetID > 0 && identityErr == nil {
					row, rowErr := st.GetConversationGTDContext(fixtureCtx, targetID)
					data["classification_metadata_available"] = rowErr == nil
					if rowErr == nil {
						revision = row.Revision
						data["target_revision"] = revision
						data["target_category"] = row.Category
						data["target_archive_hold"] = row.ArchiveHold
						data["target_needs_answer"] = row.NeedsAnswer
						data["classification_saved_this_pass"] = revision > previousRevision
						data["classification_attempt_observed"] = revision > previousRevision
					}
				}
				syncStatus := "not_requested"
				if stage == "sync" {
					syncStatus = "not_observed"
				}
				if items, ok := report["results"].([]any); ok {
					for _, item := range items {
						result, ok := item.(map[string]any)
						if !ok {
							continue
						}
						status, ok := result["status"].(string)
						if !ok {
							continue
						}
						if result["source_id"] == float64(source.ID) {
							switch status {
							case "sync_returned", "sync_failed", "sync_skipped_preview":
								syncStatus = status
							}
						}
						if targetID > 0 && result["conversation_id"] == float64(targetID) {
							// Runner statuses/context reasons are fixed metadata, not provider error bodies.
							data["target_result_present"], data["target_status"] = true, status
							if status == "classification_changed_or_unavailable" {
								data["classification_attempt_observed"] = true
							}
						}
					}
				}
				data["sync_status"] = syncStatus
				var progress struct {
					Sources map[string]struct {
						After   int64 `json:"after"`
						Ceiling int64 `json:"ceiling"`
					} `json:"sources"`
				}
				stateDir, _ := runnerConfig["state_dir"].(string)
				progressBody, progressErr := os.ReadFile(filepath.Join(stateDir, "progress.json"))
				progressOK := progressErr == nil && json.Unmarshal(progressBody, &progress) == nil
				data["progress_available"] = progressOK
				if cursor, ok := progress.Sources[strconv.FormatInt(source.ID, 10)]; progressOK && ok {
					rows, rowsErr := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
					data["local_metadata_available"] = rowsErr == nil
					if rowsErr == nil {
						afterPosition, ceilingPosition := 0, 0
						for _, row := range rows {
							if row.ConversationID <= cursor.After {
								afterPosition++
							}
							if row.ConversationID <= cursor.Ceiling {
								ceilingPosition++
							}
						}
						// Ordinal positions and relative booleans avoid printing raw identities.
						data["local_rows"], data["after_position"], data["ceiling_position"] = len(rows), afterPosition, ceilingPosition
					}
					data["cycle_wrapped"] = cursor.After == 0 && cursor.Ceiling == 0
					data["target_within_ceiling"] = targetID > 0 && cursor.Ceiling > 0 && targetID <= cursor.Ceiling
					data["target_ahead_of_cursor"] = targetID > 0 && targetID > cursor.After
				}
				provider.mu.Lock()
				requests, chatReads := provider.chatRequests["zz-native-reference"], provider.chatReads["zz-native-reference"]
				provider.mu.Unlock()
				data["target_native_requests_delta"], data["target_native_chat_reads_delta"] = requests-previousRequests, chatReads-previousChatReads
				// Request totals include sync/import reads; they are not mislabeled as context-only counts.
				previousRevision, previousRequests, previousChatReads = revision, requests, chatReads
				t.Logf("catalog_probe=%s", mustJSON(t, data))
				return data
			}
			found := false
			var lastObservation map[string]any
			for pass := range 5 {
				report, err := invoke()
				lastObservation = catalogDiagnostic("catalog", pass+1, report, err != nil)
				require.NoError(err)
				coverage, ok := report["provider_coverage"].([]any)
				require.True(ok)
				require.Len(coverage, 1)
				observation, ok := coverage[0].(map[string]any)
				require.True(ok)
				unknown, ok := observation["unknown_local_identities"].(float64)
				require.True(ok)
				found = found || strconv.FormatFloat(unknown, 'f', -1, 64) == "1"
			}
			assert.True(found, "bounded native continuation must reach the unknown identity beyond the first page and all-skipped middle page")
			identities, err := st.BeeperConversationIDsContext(fixtureCtx, source.ID, []string{"zz-native-reference"})
			require.NoError(err)
			assert.Empty(identities, "catalog observation alone must not import a chat")
			provider.mu.Lock()
			provider.catalogSkipPage = false
			provider.mu.Unlock()
			runnerConfig["sync_sources"] = true
			require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
			require.Equal(true, lastObservation["progress_available"])
			require.Equal(true, lastObservation["local_metadata_available"])
			afterPosition, afterOK := lastObservation["after_position"].(int)
			ceilingPosition, ceilingOK := lastObservation["ceiling_position"].(int)
			require.True(afterOK && ceilingOK)
			require.GreaterOrEqual(ceilingPosition, afterPosition)
			remainingOld := ceilingPosition - afterPosition
			workPerPass, workOK := runnerConfig["max_chats"].(int)
			require.True(workOK)
			require.Positive(workPerPass)
			require.Equal(workPerPass, runnerConfig["page_size"], "this fixture scans one bounded page per invocation")
			// One actual scoped sync discovers the target and current cohort.
			report, callErr := invoke()
			firstScan := catalogDiagnostic("sync", 1, report, callErr != nil)
			err = callErr
			require.NoError(err)
			require.Equal(true, firstScan["target_present"])
			require.Equal(true, firstScan["local_metadata_available"])
			currentCohort, cohortOK := firstScan["local_rows"].(int)
			require.True(cohortOK)
			require.Positive(currentCohort)
			require.Less(currentCohort, 200, "the local fixture census must fit its metadata page")
			// Finish the frozen cohort, then traverse one whole current cohort.
			// The first sync invocation above is included, not an extra retry.
			passBound := (remainingOld+workPerPass-1)/workPerPass + (currentCohort+workPerPass-1)/workPerPass
			require.Positive(passBound)
			t.Logf("catalog_bound old_remaining=%d current_cohort=%d work_per_pass=%d max_sync_passes=%d", remainingOld, currentCohort, workPerPass, passBound)
			admitted := firstScan["target_result_present"] == true
			wraps := 0
			if firstScan["cycle_wrapped"] == true {
				wraps++
			}
			for pass := 1; pass < passBound; pass++ {
				report, callErr := invoke()
				observed := catalogDiagnostic("sync", pass+1, report, callErr != nil)
				err = callErr
				require.NoError(err)
				require.Equal(currentCohort, observed["local_rows"], "the provider cohort remains fixed during this finite traversal")
				admitted = admitted || observed["target_result_present"] == true
				if observed["cycle_wrapped"] == true {
					wraps++
				}
			}
			expectedWraps := 1
			if remainingOld > 0 {
				expectedWraps++
			}
			assert.Equal(expectedWraps, wraps, "finish the frozen scan and the complete current cohort")
			assert.True(admitted, "the finite progress bound must admit the new identity")
			identities, err = st.BeeperConversationIDsContext(fixtureCtx, source.ID, []string{"zz-native-reference"})
			require.NoError(err)
			require.Positive(identities["zz-native-reference"])
			classified, err := st.GetConversationGTDContext(fixtureCtx, identities["zz-native-reference"])
			require.NoError(err)
			assert.Equal("reference", classified.Category)
			provider.mu.Lock()
			assert.True(provider.chats["zz-native-reference"].archived)
			provider.mu.Unlock()
		}
	}
	if len(reviewScenario) > 0 {
		if reviewScenario[0] == "review-matrix" {
			for _, scenario := range []string{"report", "draft", "sync", "budget", "deadline", "reconciliation", "draft-after-write"} {
				t.Run("review-"+scenario, func(t *testing.T) { runReviewCases(t, scenario) })
			}
		} else {
			t.Run(reviewScenario[0], func(t *testing.T) { runReviewCases(t, reviewScenario[0]) })
		}
		return
	}

	t.Run("incomplete native catalog remains distinct from local progress", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		configureCase("catalog")
		provider.mu.Lock()
		provider.catalogStalled = true
		beforePosts, beforeDrafts := provider.archives, provider.drafts
		provider.mu.Unlock()
		t.Cleanup(func() {
			provider.mu.Lock()
			provider.catalogStalled = false
			provider.mu.Unlock()
		})
		report, err := invoke()
		require.NoError(err)
		coverage, ok := report["provider_coverage"].([]any)
		require.True(ok, "coverage must be an array")
		require.Len(coverage, 1)
		coverageSource, ok := coverage[0].(map[string]any)
		require.True(ok, "coverage entry must be an object")
		assert.Equal("unavailable_or_invalid", coverageSource["status"])
		scanned, ok := report["scanned"].(float64)
		require.True(ok, "scanned must be numeric")
		assert.Equal(strconv.Itoa(len(rows)), strconv.FormatFloat(scanned, 'f', -1, 64), "failed catalog traversal must not stop the local scan; count must be the exact integer")
		provider.mu.Lock()
		assert.Equal(beforePosts, provider.archives)
		assert.Equal(beforeDrafts, provider.drafts)
		provider.mu.Unlock()
	})
	assertDiagnosticRerun := func(status string) {
		t.Helper()
		beforeRows, err := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, err)
		current, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(t, err)
		provider.mu.Lock()
		beforePosts, beforeDrafts := provider.archives, provider.drafts
		provider.mu.Unlock()
		stateDir, ok := runnerConfig["state_dir"].(string)
		require.True(t, ok, "state directory must be a string")
		reportPath := filepath.Join(stateDir, "report.json")
		body, err := os.ReadFile(reportPath)
		require.NoError(t, err)
		var prior map[string]any
		require.NoError(t, json.Unmarshal(body, &prior))
		priorUnresolved, ok := prior["unresolved"].([]any)
		require.True(t, ok)
		for _, raw := range priorUnresolved {
			item, ok := raw.(map[string]any)
			require.True(t, ok)
			item["body"] = "PRIVATE-DIAGNOSTIC-BODY"
			item["extra"] = map[string]any{"text": "PRIVATE-DIAGNOSTIC-BODY"}
		}
		require.NoError(t, os.WriteFile(reportPath, mustJSON(t, prior), 0600))
		report, err := invoke()
		require.NoError(t, err)
		expected := map[string]any{"conversation_id": float64(late.ConversationID), "source_id": float64(source.ID), "revision": float64(current.Revision), "status": status}
		unresolved, ok := report["unresolved"].([]any)
		require.True(t, ok)
		assert.Contains(t, unresolved, expected)
		assert.NotContains(t, string(mustJSON(t, report)), "PRIVATE-DIAGNOSTIC-BODY")
		found := false
		answerGroups, ok := report["answer_needed"].([]any)
		require.True(t, ok)
		for _, group := range answerGroups {
			answerGroup, ok := group.(map[string]any)
			require.True(t, ok)
			conversations, ok := answerGroup["conversations"].([]any)
			require.True(t, ok)
			for _, raw := range conversations {
				row, ok := raw.(map[string]any)
				require.True(t, ok)
				if row["conversation_id"] == float64(late.ConversationID) {
					assert.Equal(t, expected, row["action_required"])
					found = true
				}
			}
		}
		assert.True(t, found)
		afterRows, err := st.ListConversationGTDContext(fixtureCtx, source.ID, 0, 200, false)
		require.NoError(t, err)
		for i, row := range afterRows {
			assert.Equal(t, beforeRows[i].Revision, row.Revision)
		}
		provider.mu.Lock()
		assert.Equal(t, beforePosts, provider.archives)
		assert.Equal(t, beforeDrafts, provider.drafts)
		provider.mu.Unlock()
	}
	t.Run("complete invalid batch prevents valid first decision", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		before := resetLate()
		configureCase("invalid")
		_, err := invoke()
		require.Error(err)
		after, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		assert.Equal(before.Revision, after.Revision)
		provider.mu.Lock()
		assert.Equal(postCount, provider.archives)
		assert.Equal(draftCount, provider.drafts)
		provider.mu.Unlock()
	})
	t.Run("same activity edit during model has zero product writes", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		before := resetLate()
		configureCase("race")
		done := make(chan error, 1)
		go func() {
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-fixtureCtx.Done():
					done <- fixtureCtx.Err()
					return
				case <-deadline.C:
					done <- errors.New("fixture model did not request race")
					return
				case <-tick.C:
					body, err := os.ReadFile(filepath.Join(root, "race-request"))
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						done <- err
						return
					}
					var chatIDs []string
					if err = json.Unmarshal(body, &chatIDs); err != nil {
						done <- err
						return
					}
					provider.mu.Lock()
					for _, id := range chatIDs {
						chat := provider.chats[id]
						chat.messages[len(chat.messages)-1]["text"] = "Same-activity in-scope edit during inference"
					}
					provider.mu.Unlock()
					done <- os.WriteFile(filepath.Join(root, "race-done"), []byte("done"), 0600)
					return
				}
			}
		}()
		_, err := invoke()
		require.NoError(err)
		require.NoError(<-done)
		after, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		assert.Equal(before.Revision, after.Revision)
		provider.mu.Lock()
		assert.Equal(postCount, provider.archives)
		assert.Equal(draftCount, provider.drafts)
		provider.mu.Unlock()
	})
	t.Run("disabled requested draft leaves occupied composer and archives", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		resetLate()
		configureCase("request-draft")
		runnerConfig["prepare_drafts"] = false
		require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
		provider.mu.Lock()
		provider.chats["room4"].draft = map[string]any{"text": "Human composer text"}
		provider.mu.Unlock()
		_, err := invoke()
		require.NoError(err)
		provider.mu.Lock()
		assert.True(provider.chats["room4"].archived)
		assert.Equal(map[string]any{"text": "Human composer text"}, provider.chats["room4"].draft)
		assert.Equal(draftCount, provider.drafts)
		postCount = provider.archives
		provider.mu.Unlock()
	})
	t.Run("enabled requested draft conflict holds archive", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		resetLate()
		configureCase("request-draft")
		runnerConfig["prepare_drafts"] = true
		require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
		_, err := invoke()
		require.NoError(err)
		provider.mu.Lock()
		assert.False(provider.chats["room4"].archived)
		assert.Equal(postCount, provider.archives)
		assert.Equal(draftCount, provider.drafts)
		provider.mu.Unlock()
		assertDiagnosticRerun("draft_held")
		current, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		newer, err := st.SetConversationGTDContext(fixtureCtx, current.ConversationID, store.ConversationGTDChange{Category: "todo", ExpectedRevision: &current.Revision, ObservedActivity: current.ObservedActivity, ContextStamp: current.ContextStamp, ReviewedHead: current.ReviewedHead})
		require.NoError(err)
		// The late row is outside this candidate page, but its current group
		// projection must still supersede a diagnostic from an older revision.
		runnerConfig["page_size"] = 2
		runnerConfig["max_chats"] = 2
		require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
		report, err := invoke()
		require.NoError(err)
		unresolved, ok := report["unresolved"].([]any)
		require.True(ok)
		for _, raw := range unresolved {
			item, ok := raw.(map[string]any)
			require.True(ok)
			assert.NotEqual(float64(late.ConversationID), item["conversation_id"])
		}
		after, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		assert.Equal(newer.Revision, after.Revision)
		provider.mu.Lock()
		assert.Equal(postCount, provider.archives)
		assert.Equal(draftCount, provider.drafts)
		provider.mu.Unlock()
	})
	t.Run("definite archive rejection remains diagnostic without replay", func(t *testing.T) {
		resetLate()
		configureCase("rejected")
		runnerConfig["prepare_drafts"] = false
		require.NoError(t, os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
		provider.mu.Lock()
		provider.chats["room4"].archiveStatus = http.StatusForbidden
		provider.mu.Unlock()
		t.Cleanup(func() {
			provider.mu.Lock()
			provider.chats["room4"].archiveStatus = 0
			provider.mu.Unlock()
		})
		_, err := invoke()
		require.NoError(t, err)
		assertDiagnosticRerun("provider_rejected")
		provider.mu.Lock()
		assert.False(t, provider.chats["room4"].archived)
		postCount = provider.archives
		provider.mu.Unlock()
	})
	t.Run("current unknown hold survives restart and confirmed true reconciles", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		configureCase("hold")
		runnerConfig["prepare_drafts"] = false
		require.NoError(os.WriteFile(configPath, mustJSON(t, runnerConfig), 0600))
		current, err := st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		require.NoError(st.SetConversationGTDHoldContext(fixtureCtx, late.ConversationID, current.Revision, true))
		provider.mu.Lock()
		provider.chats["room4"].archived = false
		beforePosts := provider.archives
		provider.mu.Unlock()
		for range 2 {
			report, err := invoke()
			require.NoError(err)
			found := false
			answerGroups, ok := report["answer_needed"].([]any)
			require.True(ok)
			for _, group := range answerGroups {
				answerGroup, ok := group.(map[string]any)
				require.True(ok)
				conversations, ok := answerGroup["conversations"].([]any)
				require.True(ok)
				for _, raw := range conversations {
					row, ok := raw.(map[string]any)
					require.True(ok)
					if row["conversation_id"] == float64(late.ConversationID) {
						assert.Equal(true, row["archive_hold"])
						found = true
					}
				}
			}
			assert.True(found)
		}
		current, err = st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		assert.True(current.ArchiveHold)
		provider.mu.Lock()
		assert.Equal(beforePosts, provider.archives)
		provider.chats["room4"].archived = true
		provider.mu.Unlock()
		_, err = invoke()
		require.NoError(err)
		current, err = st.GetConversationGTDContext(fixtureCtx, late.ConversationID)
		require.NoError(err)
		assert.False(current.ArchiveHold)
		provider.mu.Lock()
		assert.Equal(beforePosts, provider.archives)
		provider.mu.Unlock()
	})
}

// Independent review faults share the real fixture, but not the unrelated
// discovery/replay loop or its finite cleanup deadline.
func TestBeeperMorningReviewBuiltCLI(t *testing.T) {
	runBeeperMorningBuiltCLI(t, "review-matrix")
}

func TestBeeperMorningClassificationCompletionBuiltCLI(t *testing.T) {
	runBeeperMorningBuiltCLI(t, "classification-ack")
}

func TestBeeperMorningNativePaginationBuiltCLI(t *testing.T) {
	runBeeperMorningBuiltCLI(t, "catalog")
}

const morningClassifierFixture = `#!/usr/bin/python3
import json,os,sys,time
if sys.argv[1:]==['--version']:
 print('1.4.0');sys.exit(0)
for name in ['MSGVAULT_API_KEY','BEEPER_ACCESS_TOKEN','NODE_OPTIONS','CODEX_HOME','SSH_AUTH_SOCK','HTTP_PROXY']:
 assert name not in os.environ,name
assert os.environ['PATH']=='/usr/bin:/bin'
assert '--no-tools' in sys.argv and '--tools' not in sys.argv
assert sys.argv[sys.argv.index('--exclude-tools')+1]=='*'
for flag in ['--no-mcp','--no-extensions','--no-skills','--no-context-files','--no-prompt-templates','--no-themes','--no-approve','--no-session']:
 assert flag in sys.argv
assert os.path.isdir(os.environ['PI_CODING_AGENT_DIR']) and not os.listdir(os.environ['PI_CODING_AGENT_DIR'])
data=json.loads(sys.stdin.read().split('\n',1)[1]);decisions=[]
root=os.path.dirname(__file__)
mode=open(os.path.join(root,'fixture-mode')).read() if os.path.exists(os.path.join(root,'fixture-mode')) else ''
for snap in data['snapshots']:
 state=snap['state'];title=state['title']
 category=next((c for c in ['waiting','delegated','reference'] if c in title),'todo')
 decision={'conversation_id':state['conversation_id'],'category':category,'needs_answer':category=='todo'}
 if title in ['old-todo','draft-todo'] or mode=='request-draft':decision['draft_text']='Prepared synthetic response'
 decisions.append(decision)
if mode=='inference-delay':time.sleep(6)
if mode=='request-budget':
 for snap,decision in zip(data['snapshots'],decisions):
  if snap['state']['chat_id'].startswith('budget-'):decision['draft_text']='Synthetic requested bounded draft'
if mode in ['report-conflict','report-rejected','sync-fairness']:
 for decision in decisions:
  decision['category']='reference';decision['needs_answer']=False
  if mode=='report-conflict':decision['draft_text']='Requested synthetic response'
if mode=='invalid':decisions.append({'conversation_id':999999,'hold':True})
if mode=='race':
 with open(os.path.join(root,'race-request'),'w') as f:json.dump([s['state']['chat_id'] for s in data['snapshots']],f)
 deadline=time.monotonic()+10
 while not os.path.exists(os.path.join(root,'race-done')):
  if time.monotonic()>deadline:sys.exit(1)
  time.sleep(0.02)
print(json.dumps({'type':'message_end','message':{'role':'assistant','provider':'zai','model':'fixture','stopReason':'stop','content':[{'type':'text','text':json.dumps({'decisions':decisions})}]}}))
print(json.dumps({'type':'agent_settled','aborted':False}))
`
