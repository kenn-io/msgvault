package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/whoknows"
)

func TestWriteWhoKnowsEmptyResults(t *testing.T) {
	for name, tc := range map[string]struct {
		response whoknows.Response
		want     string
	}{
		"no evidence": {
			whoknows.Response{Topic: "sailboat insurance", Reason: whoknows.ReasonNoEvidence},
			"Nobody in your archive has written about \"sailboat insurance\".\n",
		},
		"search limit": {
			whoknows.Response{Topic: "kubernetes", Reason: whoknows.ReasonSearchLimit},
			"No one turned up for \"kubernetes\" within the search limits; narrow with --after or --before.\n",
		},
		"index scope": {
			whoknows.Response{Topic: "kubernetes", Reason: whoknows.ReasonIndexScope},
			"The vector index doesn't cover email or chat, so who-knows couldn't search messages.\n",
		},
		"declared in part": {
			whoknows.Response{Topic: "kubernetes", Reason: whoknows.ReasonSearchLimit, DeclaredTruncated: true},
			"No one turned up for \"kubernetes\" within the search limits; narrow with --after or --before.\n" +
				"Declared topics were searched only in part.\n",
		},
	} {
		var out bytes.Buffer
		require.NoError(t, writeWhoKnows(&out, tc.response), name)
		assert.Equal(t, tc.want, out.String(), name)
	}
}

func TestWriteWhoKnowsListsCitationsDeclaredOnlyAndPromoteHint(t *testing.T) {
	sent := time.Date(2026, 9, 12, 15, 4, 0, 0, time.UTC)
	var out bytes.Buffer
	require.NoError(t, writeWhoKnows(&out, whoknows.Response{
		Topic: "kubernetes networking", SearchedMessages: 200, PoolSaturated: true,
		People: []whoknows.Person{
			{
				Rank: 1, PersonID: 7, DisplayName: "Ana Lopez", Score: 1.2, MessageScore: .45, DeclaredBoost: .75,
				AuthoredMessages: 3, AskMeAbout: []string{"kubernetes", "networking"},
				Citations: []whoknows.Citation{
					{MessageID: 4812, SentAt: &sent, MessageType: "email", Subject: "kube-proxy rules"},
					{MessageID: 4813, MessageType: "chat", Snippet: "try the \x1b[31mpolicy\x1b[0m"},
				},
			},
			{Rank: 2, PersonID: 8, DisplayName: "Lou Lurker", Score: .75, DeclaredBoost: .75, Citations: []whoknows.Citation{}},
		},
		UnsavedContacts: []whoknows.Contact{{
			Rank: 1, ParticipantID: 9, EmailAddress: "dee@example.net", Score: .62, MessageScore: .62, AuthoredMessages: 2,
			Citations: []whoknows.Citation{{MessageID: 90, SentAt: &sent, MessageType: "email", Subject: "ingress"}},
		}},
		DeclaredTruncated: true,
	}))
	assert.Equal(t, `#1  Ana Lopez  score 1.20 (messages 0.45, declared 0.75)  3 authored
    declared: kubernetes, networking
    2026-09-12  email  kube-proxy rules  msgvault show-message 4812
    undated  chat  try the policy  msgvault show-message 4813
#2  Lou Lurker  score 0.75 (messages 0.00, declared 0.75)  declared, no messages
Unsaved contacts
#1  dee@example.net  score 0.62  2 authored  msgvault person promote 9
    2026-09-12  email  ingress  msgvault show-message 90
Based on the 200 closest messages.
Declared topics were searched only in part.
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
	var body map[string]any
	out, err := runWhoKnows(t, "3.24.0", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/people/who-knows", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"topic":"kubernetes networking","reason":"no_relevant_evidence","people":[],"unsaved_contacts":[]}`))
	}, "kubernetes", "networking", "--limit", "3", "--min-score", "0.4", "--after", "2026-01-01")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"topic": "kubernetes networking", "limit": float64(3), "min_score": 0.4, "after": "2026-01-01T00:00:00Z",
	}, body)
	assert.Equal(t, "Nobody in your archive has written about \"kubernetes networking\".\n", out)
}

func TestWhoKnowsCommandExplainsAnOlderDaemon(t *testing.T) {
	_, err := runWhoKnows(t, "3.23.0", func(http.ResponseWriter, *http.Request) {
		assert.Fail(t, "an old daemon has no who-knows route")
	}, "cloud")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires daemon API schema 3.24.0 or newer")
	assert.Contains(t, err.Error(), `"3.23.0"`)
}
