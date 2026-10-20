package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestWriteWhoKnowsEmptyResults(t *testing.T) {
	for name, tc := range map[string]struct {
		response generated.WhoKnowsResponse
		want     string
	}{
		"nobody": {
			generated.WhoKnowsResponse{Topic: "sailboat insurance"},
			"Nobody in your archive has written about \"sailboat insurance\".\n",
		},
		"full pool": {
			generated.WhoKnowsResponse{Topic: "kubernetes", Limits: []generated.WhoKnowsResponseLimits{generated.PoolFull}},
			"No one turned up for \"kubernetes\".\n" +
				"Searched the 200 closest messages; more may match. Narrow with --after or --before.\n",
		},
		"index scope and declared in part": {
			generated.WhoKnowsResponse{Topic: "kubernetes", Limits: []generated.WhoKnowsResponseLimits{
				generated.IndexScope, generated.DeclaredTruncated,
			}},
			"No one turned up for \"kubernetes\".\n" +
				"The vector index doesn't cover email or chat, so messages weren't searched.\n" +
				"Declared topics were searched only in part.\n",
		},
	} {
		var out bytes.Buffer
		require.NoError(t, writeWhoKnows(&out, tc.response), name)
		assert.Equal(t, tc.want, out.String(), name)
	}
}

func TestWriteWhoKnowsListsCitationsDeclaredWordsAndPromoteHint(t *testing.T) {
	sent := time.Date(2026, 9, 12, 15, 4, 0, 0, time.UTC)
	var out bytes.Buffer
	require.NoError(t, writeWhoKnows(&out, generated.WhoKnowsResponse{
		Topic:  "kubernetes networking",
		Limits: []generated.WhoKnowsResponseLimits{generated.PoolFull},
		People: []generated.WhoKnowsEntry{
			{
				Rank: 1, PersonID: new(int64(7)), DisplayName: new("Ana Lopez"), Score: 1.2, MessageScore: .45, DeclaredBoost: .75,
				AuthoredMessages: 3, AskMeAbout: []string{"kubernetes networking"},
				Citations: []generated.WhoKnowsCitation{
					{MessageID: 4812, SentAt: &sent, MessageType: "email", Subject: new("Re: pod networking in staging")},
					{MessageID: 4813, MessageType: "chat", Subject: new("try the \x1b[31mpolicy\x1b[0m")},
				},
			},
			{
				Rank: 2, PersonID: new(int64(8)), DisplayName: new("Lou Lurker"), Score: .75, DeclaredBoost: .75,
				AskMeAbout: []string{"kubernetes networking and ingress"}, Citations: []generated.WhoKnowsCitation{},
			},
		},
		UnsavedContacts: []generated.WhoKnowsEntry{{
			Rank: 1, ParticipantID: new(int64(9)), EmailAddress: new("dee@example.net"), Score: .62, MessageScore: .62, AuthoredMessages: 2,
			Citations: []generated.WhoKnowsCitation{{MessageID: 90, SentAt: &sent, MessageType: "email", Subject: new("ingress")}},
		}},
	}))
	assert.Equal(t, `#1  Ana Lopez  score 1.20 (messages 0.45, declared 0.75)  3 authored  declared: kubernetes networking
    2026-09-12  email  Re: pod networking in staging  msgvault show-message 4812
    undated  chat  try the policy  msgvault show-message 4813
#2  Lou Lurker  score 0.75 (messages 0.00, declared 0.75)  declared, no messages  declared: kubernetes networking and ingress
Unsaved contacts
#1  dee@example.net  score 0.62  2 authored  msgvault person promote 9
    2026-09-12  email  ingress  msgvault show-message 90
Searched the 200 closest messages; more may match. Narrow with --after or --before.
`, out.String())
}

// runWhoKnows runs the command against a daemon that reports schema and
// answers the who-knows route with handler.
func runWhoKnows(t *testing.T, schema string, handler http.HandlerFunc, args ...string) (string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + schema + `"}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	command := newPersonWhoKnowsCommand()
	command.SetContext(withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}}))
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(new(bytes.Buffer))
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

func TestWhoKnowsCommandSendsOnlyFlagsTheUserSet(t *testing.T) {
	var query url.Values
	out, err := runWhoKnows(t, "3.24.0", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/people/who-knows", r.URL.Path)
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"topic":"kubernetes networking","limits":[],"people":[],"unsaved_contacts":[]}`))
	}, "kubernetes", "networking", "--limit", "3", "--min-score", "0.4", "--after", "2026-01-01")
	require.NoError(t, err)
	assert.Equal(t, url.Values{
		"topic": {"kubernetes networking"}, "limit": {"3"}, "min_score": {"0.4"}, "after": {"2026-01-01T00:00:00Z"},
	}, query)
	assert.Equal(t, "Nobody in your archive has written about \"kubernetes networking\".\n", out)
}

func TestWhoKnowsCommandPrintsTheAPIJSON(t *testing.T) {
	body := `{"topic":"cloud","limits":["pool_full"],"people":[],"unsaved_contacts":[]}`
	out, err := runWhoKnows(t, "3.24.0", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}, "cloud", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, body, out)
}

func TestWhoKnowsCommandExplainsAnOlderDaemon(t *testing.T) {
	_, err := runWhoKnows(t, "3.23.0", func(http.ResponseWriter, *http.Request) {
		assert.Fail(t, "an old daemon has no who-knows route")
	}, "cloud")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires daemon API schema 3.24.0 or newer")
	assert.Contains(t, err.Error(), `"3.23.0"`)
}
