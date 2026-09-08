package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// personBriefTestDaemon records what the CLI sent and answers with the exact
// wire shapes the daemon serves.
type personBriefTestDaemon struct {
	t        *testing.T
	requests atomic.Int32
	method   string
	path     string
	body     string
	status   int
	response string
}

func newPersonBriefTestDaemon(t *testing.T, response string) *personBriefTestDaemon {
	t.Helper()
	daemon := &personBriefTestDaemon{t: t, status: http.StatusOK, response: response}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemon.requests.Add(1)
		daemon.method = r.Method
		daemon.path = r.URL.RequestURI()
		payload, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		daemon.body = string(payload)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(daemon.status)
		_, _ = w.Write([]byte(daemon.response))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	return daemon
}

func runPersonBriefCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: personValue}
	person.AddCommand(newPersonBriefCommand())
	root.AddCommand(person)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{personValue, "brief"}, args...))
	err := root.Execute()
	return output.String(), err
}

const personBriefCLIPayload = `{
  "version": 2,
  "status": "current",
  "generated_at": "2026-08-29T18:42:10Z",
  "rendered_text": "Last time you talked (Aug 29, chat): they were preparing for a role change. You may want to ask how the transition went.",
  "sentences": [
    {"kind": "last_interaction", "index": 0,
     "text": "Last time you talked (Aug 29, chat): they were preparing for a role change."},
    {"kind": "follow_up", "index": 0, "text": "You may want to ask how the transition went."}
  ],
  "structured": {"highlights": []},
  "evidence": [
    {"ordinal": 0, "evidence_id": 11, "evidence_key": "key-1", "source_ref": "message:1",
     "source_url": "https://example.test/1", "directness": "direct-other",
     "event_time": "2026-08-29T17:00:00Z", "evidence_supported": true},
    {"ordinal": 1, "evidence_id": 12, "evidence_key": "key-2", "source_ref": "message:2",
     "source_url": "", "directness": "direct-other",
     "event_time": "2026-08-27T09:30:00Z", "evidence_supported": false}
  ],
  "boundary": {"through_sequence": 184233},
  "dropped_item_count": 1,
  "program_id": "msgvault-person-brief",
  "program_version": "v1",
  "provider": "openai_chat",
  "model": "gpt-test",
  "rejected_at": null,
  "rejected_reason": "",
  "superseded_at": null
}`

func TestPersonBriefShowPrintsParagraphVersionLineAndEvidenceDates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	daemon := newPersonBriefTestDaemon(t, personBriefCLIPayload)

	output, err := runPersonBriefCommand(t, "show", "7")
	requirements.NoError(err)
	assertions.Equal(http.MethodGet, daemon.method)
	assertions.Equal("/api/v1/people/7/brief", daemon.path)
	assertions.Contains(output, "Person 7 brief version 2 (current, generated 2026-08-29)")
	assertions.Contains(output,
		"Last time you talked (Aug 29, chat): they were preparing for a role change.")
	assertions.Contains(output, "You may want to ask how the transition went.")
	assertions.Contains(output, "2026-08-29")
	assertions.Contains(output, "2026-08-27")
	assertions.Contains(output, "unsupported")
}

func TestPersonBriefShowJSONPassesTheDaemonBodyThrough(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	newPersonBriefTestDaemon(t, personBriefCLIPayload)

	output, err := runPersonBriefCommand(t, "show", "7", "--json")
	requirements.NoError(err)
	var decoded map[string]any
	requirements.NoError(json.Unmarshal([]byte(output), &decoded))
	assertions.EqualValues(2, decoded["version"])
	assertions.Equal("current", decoded["status"])
	assertions.Contains(decoded, "sentences")
	assertions.Contains(decoded, "evidence")
}

func TestPersonBriefShowReportsAMissingVersion(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t,
		`{"error":"person_brief_not_found","message":"Person brief not found"}`)
	daemon.status = http.StatusNotFound

	_, err := runPersonBriefCommand(t, "show", "7")
	require.Error(t, err)
	assert.ErrorContains(t, err, "Person brief not found")
}

func TestPersonBriefHistoryPrintsOneLinePerVersion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	daemon := newPersonBriefTestDaemon(t, `{"versions":[
		{"version":2,"status":"current","generated_at":"2026-08-29T18:42:10Z",
		 "rendered_text":"Newer.","sentences":[],"structured":{},"evidence":[],
		 "boundary":{},"dropped_item_count":0,"program_id":"msgvault-person-brief",
		 "program_version":"v1","provider":"openai_chat","model":"gpt-test",
		 "rejected_at":null,"rejected_reason":"","superseded_at":null},
		{"version":1,"status":"superseded","generated_at":"2026-08-22T10:00:00Z",
		 "rendered_text":"Older.","sentences":[],"structured":{},"evidence":[],
		 "boundary":{},"dropped_item_count":2,"program_id":"msgvault-person-brief",
		 "program_version":"v1","provider":"openai_chat","model":"gpt-test",
		 "rejected_at":null,"rejected_reason":"","superseded_at":"2026-08-29T18:42:10Z"}]}`)

	output, err := runPersonBriefCommand(t, "history", "7")
	requirements.NoError(err)
	assertions.Equal("/api/v1/people/7/brief/versions", daemon.path)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	requirements.Len(lines, 3, "a header and one line per version")
	assertions.Contains(lines[0], "VERSION")
	assertions.Contains(lines[1], "current")
	assertions.Contains(lines[1], "2026-08-29")
	assertions.Contains(lines[2], "superseded")
	assertions.Contains(lines[2], "2026-08-22")
}

func TestPersonBriefHistoryHonorsLimit(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{"versions":[]}`)

	output, err := runPersonBriefCommand(t, "history", "7", "--limit", "5")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/people/7/brief/versions?limit=5", daemon.path)
	assert.Contains(t, output, "VERSION")
}

func TestPersonBriefRejectSendsTheReason(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	daemon := newPersonBriefTestDaemon(t, `{
		"version":2,"status":"rejected","generated_at":"2026-08-29T18:42:10Z",
		"rendered_text":"Paragraph.","sentences":[],"structured":{},"evidence":[],
		"boundary":{},"dropped_item_count":0,"program_id":"msgvault-person-brief",
		"program_version":"v1","provider":"openai_chat","model":"gpt-test",
		"rejected_at":"2026-08-30T09:00:00Z","rejected_reason":"wrong thread",
		"superseded_at":null}`)

	output, err := runPersonBriefCommand(t, "reject", "7", "--reason", "wrong thread")
	requirements.NoError(err)
	assertions.Equal(http.MethodPost, daemon.method)
	assertions.Equal("/api/v1/people/7/brief/reject", daemon.path)
	assertions.JSONEq(`{"reason":"wrong thread"}`, daemon.body)
	assertions.Contains(output, "Person 7 brief version 2: rejected")
	assertions.Contains(output, "wrong thread")
}

func TestPersonBriefGenerateReportsTheRunAndWarnsAboutSpend(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	daemon := newPersonBriefTestDaemon(t,
		`{"run_id":"run-1","attempt_id":"attempt-1","brief_version":3,
		  "brief_failure_class":""}`)

	output, err := runPersonBriefCommand(t, "generate", "7")
	requirements.NoError(err)
	assertions.Equal(http.MethodPost, daemon.method)
	assertions.Equal("/api/v1/people/7/brief/generate", daemon.path)
	assertions.Contains(output, "run-1")
	assertions.Contains(output, "attempt-1")
	assertions.Contains(output, "version 3")

	help, err := runPersonBriefCommand(t, "generate", "--help")
	requirements.NoError(err)
	assertions.Contains(help, "extraction page")
	assertions.Contains(help, "budget")
}

func TestPersonBriefGenerateReportsADeferredBrief(t *testing.T) {
	newPersonBriefTestDaemon(t,
		`{"run_id":"run-2","attempt_id":"attempt-2","brief_version":0,
		  "brief_failure_class":"budget"}`)

	output, err := runPersonBriefCommand(t, "generate", "7")
	require.NoError(t, err)
	assert.Contains(t, output, "no new version")
	assert.Contains(t, output, "budget")
}

func TestPersonBriefEnrollAndUnenrollReplaceState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	daemon := newPersonBriefTestDaemon(t,
		`{"person_id":7,"enrolled":true,"enabled_at":"2026-08-29T18:42:10Z","actor":"api"}`)

	output, err := runPersonBriefCommand(t, "enroll", "7", "--track")
	requirements.NoError(err)
	assertions.Equal(http.MethodPut, daemon.method)
	assertions.Equal("/api/v1/people/7/brief-enrollment", daemon.path)
	assertions.JSONEq(`{"enrolled":true,"track":true}`, daemon.body)
	assertions.Contains(output, "Person 7 brief: enrolled")

	daemon.response = `{"person_id":7,"enrolled":true,"enabled_at":"2026-08-29T18:42:10Z","actor":"api"}`
	_, err = runPersonBriefCommand(t, "enroll", "7")
	requirements.NoError(err)
	assertions.JSONEq(`{"enrolled":true,"track":false}`, daemon.body)

	daemon.response = `{"person_id":7,"enrolled":false,"enabled_at":null,"actor":""}`
	output, err = runPersonBriefCommand(t, "unenroll", "7")
	requirements.NoError(err)
	assertions.JSONEq(`{"enrolled":false,"track":false}`, daemon.body)
	assertions.Contains(output, "Person 7 brief: not enrolled")

	jsonOutput, err := runPersonBriefCommand(t, "unenroll", "7", "--json")
	requirements.NoError(err)
	assertions.JSONEq(
		`{"person_id":7,"enrolled":false,"enabled_at":null,"actor":""}`, jsonOutput)
}

func TestPersonBriefEnrollReportsAnUntrackedPerson(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{"error":"person_brief_not_tracked",
		"message":"person 7 must be tracked before brief enrollment: run `+
		"`msgvault person track 7`"+` first, or enroll with --track"}`)
	daemon.status = http.StatusConflict

	_, err := runPersonBriefCommand(t, "enroll", "7")
	require.Error(t, err)
	assert.ErrorContains(t, err, "msgvault person track")
}

func TestPersonBriefRejectsInvalidPersonIDBeforeNetwork(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{}`)

	for _, name := range []string{"show", "history", "generate", "reject", "enroll", "unenroll"} {
		_, err := runPersonBriefCommand(t, name, "0")
		require.Error(t, err, name)
		require.ErrorContains(t, err, "positive integer", name)
	}
	assert.Zero(t, daemon.requests.Load())
}

func TestPersonBriefIsRegisteredUnderPerson(t *testing.T) {
	var brief *cobra.Command
	for _, command := range personCmd.Commands() {
		if command.Name() == "brief" {
			brief = command
		}
	}
	require.NotNil(t, brief, "person brief is registered next to person track")
	names := make([]string, 0, len(brief.Commands()))
	for _, command := range brief.Commands() {
		names = append(names, command.Name())
		assert.NotNil(t, command.Flags().Lookup(flagJSON), command.Name()+" needs --json")
	}
	assert.ElementsMatch(t,
		[]string{"show", "history", "generate", "reject", "enroll", "unenroll"}, names)
}
