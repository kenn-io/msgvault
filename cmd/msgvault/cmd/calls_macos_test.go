package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCallsImportMacOSThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "history.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE REAL,ZANSWERED INTEGER,ZADDRESS TEXT); INSERT INTO ZCALLRECORD VALUES(1,'example-call',0.125,1,'peer@example.com')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	emailSource, err := st.GetOrCreateSource("gmail", "synthetic@example.org")
	require.NoError(err)
	emailConversationID, err := st.EnsureConversation(emailSource.ID, "synthetic-email", "Synthetic Email")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  emailConversationID,
		SourceID:        emailSource.ID,
		SourceMessageID: "synthetic-email",
		MessageType:     "email",
		Subject:         sql.NullString{String: "Synthetic email", Valid: true},
	})
	require.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-key"}}
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler)}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	defer func() { _ = client.Close() }()
	run := func(args ...string) (string, error) {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&out)
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		return out.String(), err
	}
	output, err := run("import-macos", "--db", path, "--account", "example")
	require.NoError(err, output)
	assert.JSONEq(`{"imported":1}`, output)
	page, err := client.ListCallObservations(t.Context(), store.CallQuery{})
	require.NoError(err)
	require.Len(page.Items, 1)
	o := page.Items[0]
	assert.Equal("answered", o.Outcome)
	assert.Empty(o.Participants)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/cli/message/raw?id=%d", server.URL, o.MessageID), nil)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer "+cfg.Server.APIKey)
	resp, err := server.Client().Do(req)
	require.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(http.StatusOK, resp.StatusCode)
	var raw map[string]any
	require.NoError(json.UnmarshalRead(resp.Body, &raw))
	assert.Equal("peer@example.com", raw["ZADDRESS"])
	peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "manual")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	manual := callhistory.Observation{Evidence: o.Evidence, RecordID: "manual-attribution", Raw: []byte(`{}`), Participants: []callhistory.Participant{{ID: peer, Participation: "joined"}}}
	record, err := json.Marshal(manual)
	require.NoError(err)
	wire, err := json.Marshal(callhistory.ImportRequest{Format: "normalized", Account: "example", Record: record})
	require.NoError(err)
	_, err = client.ImportCallObservation(t.Context(), wire)
	require.NoError(err)
	output, err = run("import-macos", "--db", path, "--account", "example")
	require.NoError(err, output)
	page, err = client.ListCallObservations(t.Context(), store.CallQuery{})
	require.NoError(err)
	assert.Len(page.Items, 2)
	summary, err := client.SummarizeCalls(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(err)
	require.Len(summary.Items, 1)
	assert.Equal(1, summary.Items[0].Answered)

	defer resetSearchFlags()
	searchCfg := &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true,
	}}
	searchRoot := newTestRootCmd()
	searchRoot.SetContext(testInvocationContext(t.Context(), searchCfg, invocationOptions{}))
	searchRoot.AddCommand(searchCmd)
	searchRoot.SetArgs([]string{"search", "--message-type", callhistory.MessageType, "--json"})
	searchStdout := captureStdout(t)
	searchErr := searchRoot.Execute()
	searchOutput := searchStdout()
	require.NoError(searchErr, searchOutput)
	var searchResults []struct {
		ID int64 `json:"id"`
	}
	require.NoError(json.Unmarshal([]byte(searchOutput), &searchResults))
	require.Len(searchResults, len(page.Items))
	wantIDs := []int64{page.Items[0].MessageID, page.Items[1].MessageID}
	gotIDs := make([]int64, 0, len(searchResults))
	for _, result := range searchResults {
		gotIDs = append(gotIDs, result.ID)
	}
	assert.ElementsMatch(wantIDs, gotIDs)

	_, err = db.Exec(`INSERT INTO ZCALLRECORD VALUES(2,'invalid',NULL,1,NULL)`)
	require.NoError(err)
	_, err = run("import-macos", "--db", path, "--account", "example")
	require.ErrorContains(err, "after 1 imported")
	assert.Contains(err.Error(), "rerun")
}

func TestCallsImportMacOSLocalFailures(t *testing.T) {
	for _, args := range [][]string{{"import-macos"}, {"import-macos", "--account", " "}, {"import-macos", "--account", "example", "--db", filepath.Join(t.TempDir(), "missing")}} {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			require.FailNow(t, "local failure must not open daemon")
			return nil, nil, nil
		}})
		command.SetArgs(args)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		require.Error(t, command.ExecuteContext(t.Context()))
	}
}
