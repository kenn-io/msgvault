package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const beeperDraftTestChat = "!chat:beeper.local"

// fakeBeeperDrafts models Beeper Desktop's composer: setting text over a
// draft is refused with 409, and accepted text comes back formatted.
type fakeBeeperDrafts struct {
	mu       sync.Mutex
	account  string
	draft    string // raw JSON value of the chat's draft field
	requests int
	patches  int
	failText bool // answer 500 to the next text write without applying it
}

func (f *fakeBeeperDrafts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.URL.Path != "/v1/chats/"+beeperDraftTestChat || r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPatch {
		f.patches++
		var body struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		data, _ := io.ReadAll(r.Body)
		if json.Unmarshal(data, &body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case body.Draft == nil:
			f.draft = "null"
		case f.failText:
			f.failText = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		case f.draft != "null":
			w.WriteHeader(http.StatusConflict)
			return
		default:
			formatted, err := json.Marshal(map[string]string{"text": "<p>" + body.Draft.Text + "</p>"})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.draft = string(formatted)
		}
	}
	_, _ = io.WriteString(w, `{"id":"`+beeperDraftTestChat+`","accountID":"`+f.account+`","draft":`+f.draft+`}`)
}

func (f *fakeBeeperDrafts) set(draft string, failText bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.draft, f.failText = draft, failText
}

func (f *fakeBeeperDrafts) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draft
}

func (f *fakeBeeperDrafts) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.patches
}

type beeperDraftFixture struct {
	store   *store.Store
	source  *store.Source
	beeper  *fakeBeeperDrafts
	adapter *storeAPIAdapter
}

func newBeeperDraftFixture(t *testing.T) beeperDraftFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "whatsapp")
	require.NoError(t, err)
	fake := &fakeBeeperDrafts{account: source.Identifier, draft: "null"}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	cfg.Beeper.URL = server.URL
	require.NoError(t, beeper.SaveToken(cfg.TokensDir(), "test-token"))
	adapter := &storeAPIAdapter{
		store: st, config: cfg, logger: testLoggerValue(),
		beeperDraftPolicy: []config.GmailDraftSource{{SourceID: source.ID, Enabled: true}},
	}
	return beeperDraftFixture{store: st, source: source, beeper: fake, adapter: adapter}
}

func (f beeperDraftFixture) run(t *testing.T, grant *agentgrant.Grant, args ...string) (beeperDraftOutput, error) {
	t.Helper()
	args = append(args, "--json")
	var events []api.CLIRunEvent
	emit := func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	}
	req := api.CLIRunRequest{Args: args, Grant: grant}
	var err error
	if args[0] == api.CLIRunDraftComposeCommand {
		err = f.adapter.runCLIComposeDraft(t.Context(), req, emit)
	} else {
		err = f.adapter.runCLIDraftLifecycle(t.Context(), req, emit)
	}
	var output beeperDraftOutput
	if len(events) > 0 {
		require.NoError(t, json.Unmarshal([]byte(events[len(events)-1].Data), &output))
	}
	return output, err
}

func (f beeperDraftFixture) create(t *testing.T, grant *agentgrant.Grant, body string) (beeperDraftOutput, error) {
	t.Helper()
	return f.run(t, grant, api.CLIRunDraftComposeCommand, "--source-id", strconv.FormatInt(f.source.ID, 10),
		"--to", beeperDraftTestChat, "--body", body)
}

func (f beeperDraftFixture) edit(t *testing.T, draft beeperDraftOutput, body string) (beeperDraftOutput, error) {
	t.Helper()
	return f.run(t, nil, api.CLIRunDraftEditCommand, draft.DraftID, "--revision", strconv.FormatInt(draft.Revision, 10), "--body", body)
}

func assertBeeperDraftCode(t *testing.T, err error, code string) {
	t.Helper()
	var coded *api.CLIRunCodedError
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, code, coded.Code)
}

func TestBeeperDraftWritesCheckTheComposerFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBeeperDraftFixture(t)

	created, err := f.create(t, nil, "hello")
	require.NoError(err)
	assert.Equal("created", created.Status)
	assert.Equal(int64(1), created.Revision)
	require.NotNil(created.Content)
	assert.Equal("<p>hello</p>", *created.Content)

	_, err = f.create(t, nil, "again")
	assertBeeperDraftCode(t, err, "draft_exists")

	edited, err := f.edit(t, created, "second")
	require.NoError(err)
	assert.Equal(int64(2), edited.Revision)
	assert.Equal("<p>second</p>", *edited.Content)

	// Text typed in Beeper is never replaced or cleared.
	f.beeper.set(`{"text":"typed in Beeper"}`, false)
	_, patchesBefore := f.beeper.counts()
	_, err = f.edit(t, edited, "third")
	assertBeeperDraftCode(t, err, "draft_conflict")
	_, err = f.run(t, nil, api.CLIRunDraftDeleteCommand, edited.DraftID, "--revision", "2")
	assertBeeperDraftCode(t, err, "draft_conflict")
	_, patchesAfter := f.beeper.counts()
	assert.Equal(patchesBefore, patchesAfter)

	// An interrupted edit stays pending until Beeper shows how it ended.
	f.beeper.set(`{"text":"<p>second</p>"}`, true)
	pending, err := f.edit(t, edited, "third")
	assertBeeperDraftCode(t, err, "remote_unknown")
	assert.Equal(store.BeeperDraftOperationEdit, pending.PendingOperation)
	assert.Equal("third", pending.CandidateContent)
	retried, err := f.edit(t, edited, "third")
	require.NoError(err)
	assert.Equal(int64(3), retried.Revision)
	assert.Equal("<p>third</p>", *retried.Content)
	assert.Empty(retried.PendingOperation)

	deleted, err := f.run(t, nil, api.CLIRunDraftDeleteCommand, retried.DraftID, "--revision", "3")
	require.NoError(err)
	assert.Equal("discarded", deleted.Lifecycle)
	assert.Equal("null", f.beeper.current())

	recreated, err := f.create(t, nil, "fresh")
	require.NoError(err)
	assert.NotEqual(created.DraftID, recreated.DraftID)
	requestsBefore, _ := f.beeper.counts()
	got, err := f.run(t, nil, api.CLIRunDraftGetCommand, recreated.DraftID)
	require.NoError(err)
	assert.Equal("<p>fresh</p>", *got.Content)
	requestsAfter, _ := f.beeper.counts()
	assert.Equal(requestsBefore, requestsAfter)
}

func TestBeeperDraftDelegatedLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newBeeperDraftFixture(t)
	grant := &agentgrant.Grant{
		ID:          "beeper-grant",
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete},
		Sources:     []agentgrant.SourceRef{{Type: "beeper", Identifier: f.source.Identifier}},
	}
	created, err := f.create(t, grant, "hello")
	require.NoError(err)
	edited, err := f.run(t, grant, api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "edited")
	require.NoError(err)
	assert.JSONEq(`{"text":"<p>edited</p>"}`, f.beeper.current())
	loaded, err := f.run(t, grant, api.CLIRunDraftGetCommand, created.DraftID)
	require.NoError(err)
	require.NotNil(loaded.Content)
	assert.Equal("<p>edited</p>", *loaded.Content)
	_, err = f.run(t, grant, api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", strconv.FormatInt(edited.Revision, 10))
	require.NoError(err)
	assert.Equal("null", f.beeper.current())
}

func TestBeeperDraftDelegatedDenialPrecedesProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBeeperDraftFixture(t)
	created, err := f.create(t, nil, "hello")
	require.NoError(err)
	requestsBefore, _ := f.beeper.counts()

	other := &agentgrant.Grant{
		ID:          "beeper-grant",
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit},
		Sources:     []agentgrant.SourceRef{{Type: "beeper", Identifier: "telegram"}},
	}
	_, err = f.create(t, other, "agent")
	assertBeeperDraftCode(t, err, "not_permitted")
	_, err = f.run(t, other, api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "agent")
	assertBeeperDraftCode(t, err, "not_permitted")
	requestsAfter, _ := f.beeper.counts()
	assert.Equal(requestsBefore, requestsAfter)

	creator := &agentgrant.Grant{
		ID:          "beeper-grant",
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		Sources:     []agentgrant.SourceRef{{Type: "beeper", Identifier: f.source.Identifier}},
	}
	_, err = f.run(t, creator, api.CLIRunDraftGetCommand, created.DraftID)
	assertBeeperDraftCode(t, err, "not_permitted")
	existing, err := f.create(t, creator, "agent")
	assertBeeperDraftCode(t, err, "draft_exists")
	assert.Equal(created.DraftID, existing.DraftID)
	assert.Nil(existing.Content)
}
