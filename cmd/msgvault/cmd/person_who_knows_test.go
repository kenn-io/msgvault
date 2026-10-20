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

func TestWriteWhoKnowsEmptyResult(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeWhoKnows(&out, whoknows.Response{Topic: "sailboat insurance"}))
	assert.Equal(t, "Nobody in your archive has written about \"sailboat insurance\".\n", out.String())
}

func TestWriteWhoKnowsListsCitationsAndPromoteHint(t *testing.T) {
	sent := time.Date(2026, 9, 12, 15, 4, 0, 0, time.UTC)
	var out bytes.Buffer
	require.NoError(t, writeWhoKnows(&out, whoknows.Response{
		Topic: "kubernetes networking",
		People: []whoknows.Person{{
			Rank: 1, PersonID: 7, DisplayName: "Ana Lopez", Score: 2.314, AuthoredMessages: 6,
			Citations: []whoknows.Citation{
				{MessageID: 4812, SentAt: &sent, MessageType: "email", Subject: "kube-proxy rules"},
				{MessageID: 4813, MessageType: "chat", Snippet: "try the \x1b[31mpolicy\x1b[0m"},
			},
		}},
		UnsavedContacts: []whoknows.Contact{{
			Rank: 1, ParticipantID: 4812, DisplayName: "Ben Cho", EmailAddress: "ben@example.net",
			Score: 1.8, AuthoredMessages: 9,
			Citations: []whoknows.Citation{{MessageID: 90, SentAt: &sent, MessageType: "email", Subject: "ingress"}},
		}},
	}))
	assert.Equal(t, `#1  Ana Lopez  score 2.31  6 authored
    2026-09-12  email  kube-proxy rules  msgvault show-message 4812
    undated  chat  try the policy  msgvault show-message 4813
Unsaved contacts
#1  Ben Cho <ben@example.net>  score 1.80  9 authored  msgvault person promote 4812
    2026-09-12  email  ingress  msgvault show-message 90
`, out.String())
}

func runWhoKnows(t *testing.T, handler http.HandlerFunc, args ...string) (string, error) {
	t.Helper()
	server := httptest.NewServer(handler)
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
	out, err := runWhoKnows(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/people/who-knows", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"topic":"kubernetes networking","reason":"no_relevant_evidence","people":[],"unsaved_contacts":[]}`))
	}, "kubernetes", "networking", "--limit", "3", "--min-score", "0.4", "--after", "2026-01-01")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"topic": "kubernetes networking", "limit": float64(3), "min_score": 0.4, "after": "2026-01-01",
	}, body)
	assert.Equal(t, "Nobody in your archive has written about \"kubernetes networking\".\n", out)
}

func TestWhoKnowsCommandJSONPreservesDaemonBody(t *testing.T) {
	const body = "{ \"topic\": \"cloud\", \"people\": [], \"unsaved_contacts\": [] }\n"
	out, err := runWhoKnows(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}, "cloud", "--json")
	require.NoError(t, err)
	assert.Equal(t, body, out) //nolint:testifylint // The CLI must pass the daemon's bytes through unchanged.
}

func TestWhoKnowsCommandRejectsBadFlagsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"cloud", "--min-score", "0"},
		{"cloud", "--min-score", "1.5"},
		{"cloud", "--limit", "51"},
		{"cloud", "--evidence", "0"},
		{"cloud", "--after", "2026-02-01", "--before", "2026-01-01"},
	} {
		_, err := runWhoKnows(t, func(http.ResponseWriter, *http.Request) {
			assert.Fail(t, "the command must not reach the daemon", args)
		}, args...)
		assert.ErrorIs(t, err, whoknows.ErrInvalidParameter, args)
	}
}
