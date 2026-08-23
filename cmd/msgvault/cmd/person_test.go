package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestWriteCLIPersonSanitizesTerminalControls(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	malicious := "\x1b[31mAlice\x1b[0m \x1b]8;;https://attacker.test\x07Example\x1b]8;;\x07"
	savedJSON := personJSON
	personJSON = false
	t.Cleanup(func() { personJSON = savedJSON })

	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	require.NoError(writeCLIPerson(cmd, &generated.Person{
		ID: 7, DisplayName: &malicious, VcardUID: malicious,
	}))
	assert.NotContains(stdout.String(), "\x1b")
	assert.NotContains(stdout.String(), "https://attacker.test")
	assert.Contains(stdout.String(), "Alice Example")
}

func TestPersonPromoteAcceptsCreatedResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var participantID int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/people", r.URL.Path)
		var body struct {
			ParticipantID int64 `json:"participant_id"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		participantID = body.ParticipantID
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, err := w.Write([]byte(`{
			"id": 7,
			"vcard_uid": "17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
			"revision": 1,
			"participant_ids": [42],
			"created_at": "2026-07-29T12:00:00Z",
			"updated_at": "2026-07-29T12:00:00Z"
		}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON := personJSON
	personJSON = false
	t.Cleanup(func() { personJSON = savedJSON })

	var output bytes.Buffer
	command := &cobra.Command{
		Use:  personPromoteCmd.Use,
		Args: personPromoteCmd.Args,
		RunE: personPromoteCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"42"})

	require.NoError(command.Execute())
	assert.Equal(int64(42), participantID)
	assert.Contains(output.String(), "Person: 7")
	assert.Contains(output.String(), "Revision: 1")
}

func TestPersonSetDisplayNameClearSendsNull(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/people/7", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{
				"id":7,"vcard_uid":"17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"display_name":"alice","revision":3,"participant_ids":[42],
				"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:00:00Z"
			}`))
			assert.NoError(err)
		case http.MethodPatch:
			assert.Equal(`"person-7-r3"`, r.Header.Get("If-Match"))
			var body map[string]json.RawMessage
			if assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				assert.Equal("null", string(bytes.TrimSpace(body["display_name"])))
			}
			_, err := w.Write([]byte(`{
				"id":7,"vcard_uid":"17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"revision":4,"participant_ids":[42],
				"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:01:00Z"
			}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON, savedClear := personJSON, personClearDisplayName
	personJSON, personClearDisplayName = false, false
	t.Cleanup(func() {
		personJSON, personClearDisplayName = savedJSON, savedClear
	})

	var output bytes.Buffer
	command := &cobra.Command{
		Use:  personSetDisplayNameCmd.Use,
		Args: personSetDisplayNameCmd.Args,
		RunE: personSetDisplayNameCmd.RunE,
	}
	command.Flags().BoolVar(&personClearDisplayName, "clear", false, "")
	command.SetOut(&output)
	command.SetArgs([]string{"7", "--clear"})

	require.NoError(command.Execute())
	assert.Equal(int32(2), requests.Load())
	assert.Contains(output.String(), "Display name: -")

	err := personSetDisplayNameCmd.Args(command, []string{"7", "alice"})
	assert.ErrorContains(err, "--clear cannot be used with a display name")
}

func TestPersonDeleteSendsIfMatchFromLatestRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/people/7", r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{
				"id":7,"vcard_uid":"17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"revision":3,"participant_ids":[42],
				"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:00:00Z"
			}`))
			assert.NoError(err)
		case http.MethodDelete:
			assert.Equal(`"person-7-r3"`, r.Header.Get("If-Match"))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	var output bytes.Buffer
	command := &cobra.Command{
		Use:  personDeleteCmd.Use,
		Args: personDeleteCmd.Args,
		RunE: personDeleteCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"7"})

	require.NoError(command.Execute())
	assert.Equal(int32(2), requests.Load())
	assert.Contains(output.String(), "Deleted person 7")
}

func executePersonMergeCLI(t *testing.T, command *cobra.Command, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	require.NoError(t, command.Execute(), output.String())
	return output.String()
}

func TestPersonMergeCommandsUseConfiguredRemote(t *testing.T) {
	assertions := assert.New(t)
	requests := map[string]int{}
	splitParticipants := [][]int64{}
	personJSON := `{
		"id":7,"vcard_uid":"survivor-uid","revision":4,"participant_ids":[70,90],
		"created_at":"2026-08-19T00:00:00Z","updated_at":"2026-08-19T00:01:00Z"}`
	mergeJSON := `{
		"id":12,"survivor_person_id":7,"absorbed_person_id":9,"current_person_id":7,
		"survivor_vcard_uid":"survivor-uid","absorbed_vcard_uid":"absorbed-uid",
		"survivor_revision_before":3,"absorbed_revision_before":2,
		"survivor_revision_after":4,"actor":"api","snapshot_version":1,
		"snapshot_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"created_at":"2026-08-19T00:01:00Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		requests[key]++
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "POST /api/v1/people/7/merge":
			assertions.Equal(`"person-7-r3", "person-9-r2"`, r.Header.Get("If-Match"))
			assertions.Equal("remote-merge", r.Header.Get("Idempotency-Key"))
			var body struct {
				AbsorbedPersonID int64 `json:"absorbed_person_id"`
			}
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid merge request", http.StatusBadRequest)
				return
			}
			assertions.Equal(int64(9), body.AbsorbedPersonID)
			_, _ = fmt.Fprintf(w, `{"person":%s,"merge":%s,"review_candidates":[],
				"identity_revision":42,"cache_state":"ready"}`,
				personJSON, mergeJSON)
		case "POST /api/v1/people/7/split":
			assertions.Equal(`"person-7-r4"`, r.Header.Get("If-Match"))
			var body struct {
				MergeID        int64   `json:"merge_id"`
				ParticipantIDs []int64 `json:"participant_ids"`
			}
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid split request", http.StatusBadRequest)
				return
			}
			assertions.Equal(int64(12), body.MergeID)
			splitParticipants = append(splitParticipants, body.ParticipantIDs)
			if len(body.ParticipantIDs) == 0 {
				assertions.Equal("remote-root-split", r.Header.Get("Idempotency-Key"))
			} else {
				assertions.Equal("remote-split", r.Header.Get("Idempotency-Key"))
			}
			_, _ = fmt.Fprintf(w, `{
				"source_person":%s,
				"new_person":{"id":10,"vcard_uid":"new-uid","revision":1,
					"participant_ids":[90],"created_at":"2026-08-19T00:02:00Z",
					"updated_at":"2026-08-19T00:02:00Z"},
				"split":{"id":13,"merge_id":12,"source_person_id":7,"new_person_id":10,
					"new_person_uid":"new-uid","source_revision_before":4,
					"source_revision_after":5,"actor":"api","exact_reversal":true,
					"created_at":"2026-08-19T00:02:00Z"},
				"exact_reversal":true,"uid_alias_disposition":"retargeted","ambiguous_rows":[],
				"identity_revision":43,"cache_state":"stale"}`,
				personJSON)
		case "GET /api/v1/people/7/merges":
			_, _ = w.Write([]byte(`{"merges":[]}`))
		case "GET /api/v1/person-merges/12":
			_, _ = fmt.Fprintf(w, `{"merge":%s,"participants":[],"rows":[],"splits":[],"review_candidates":[]}`,
				mergeJSON)
		case "GET /api/v1/person-merges/12/snapshot":
			_, _ = w.Write([]byte(`{
				"version":1,
				"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"snapshot":{"persons":[{"id":7}],"rows":{"person_names":[1]}}
			}`))
		case "POST /api/v1/person-merge-candidates/21/decision":
			assertions.Equal(`"person-7-r4"`, r.Header.Get("If-Match"))
			var body struct {
				PersonID int64  `json:"person_id"`
				Decision string `json:"decision"`
			}
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid candidate request", http.StatusBadRequest)
				return
			}
			assertions.Equal(int64(7), body.PersonID)
			assertions.Equal("reject", body.Decision)
			w.Header().Set("ETag", `"person-7-r5"`)
			_, _ = w.Write([]byte(`{
				"id":21,"merge_id":12,"person_id":7,"definition_id":4,
				"survivor_value_id":31,"absorbed_value_id":32,"state":"rejected",
				"reviewed_by":"api","reviewed_at":"2026-08-19T00:03:00Z",
				"created_at":"2026-08-19T00:01:00Z"}`))
		default:
			http.Error(w, key, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	mergeJSONOutput := executePersonMergeCLI(t, newPersonMergeCommand(), "7", "9",
		"--survivor-revision", "3", "--absorbed-revision", "2",
		"--idempotency-key", "remote-merge", "--json")
	assertions.Contains(mergeJSONOutput, `"review_candidates":[]`)
	mergeOutput := executePersonMergeCLI(t, newPersonMergeCommand(), "7", "9",
		"--survivor-revision", "3", "--absorbed-revision", "2",
		"--idempotency-key", "remote-merge")
	assertions.Contains(mergeOutput, "Merge: 12")
	assertions.Contains(mergeOutput, "Absorbed UID: absorbed-uid")
	assertions.Contains(mergeOutput, "Identity revision: 42")
	assertions.Contains(mergeOutput, "Cache state: ready")
	splitJSONOutput := executePersonMergeCLI(t, newPersonSplitCommand(), "7", "--merge-id", "12",
		"--participant", "90", "--revision", "4",
		"--idempotency-key", "remote-split", "--json")
	assertions.Contains(splitJSONOutput, `"ambiguous_rows":[]`)
	splitOutput := executePersonMergeCLI(t, newPersonSplitCommand(), "7", "--merge-id", "12",
		"--participant", "90", "--revision", "4",
		"--idempotency-key", "remote-split")
	assertions.Contains(splitOutput, "Split: 13")
	assertions.Contains(splitOutput, "Exact reversal: true")
	assertions.Contains(splitOutput, "Identity revision: 43")
	assertions.Contains(splitOutput, "Cache state: stale")
	rootSplitOutput := executePersonMergeCLI(t, newPersonSplitCommand(), "7", "--merge-id", "12",
		"--revision", "4", "--idempotency-key", "remote-root-split", "--json")
	assertions.Contains(rootSplitOutput, `"exact_reversal":true`)
	assertions.Equal([][]int64{{90}, {90}, nil}, splitParticipants)
	executePersonMergeCLI(t, newPersonMergeHistoryCommand(), "7", "--json")
	assertions.Contains(executePersonMergeCLI(t, newPersonMergeHistoryCommand(), "7"), "MERGE")
	detailJSONOutput := executePersonMergeCLI(t, newPersonMergeShowCommand(), "12", "--json")
	for _, field := range []string{"participants", "rows", "splits", "review_candidates"} {
		assertions.Contains(detailJSONOutput, `"`+field+`":[]`)
	}
	assertions.Contains(executePersonMergeCLI(t, newPersonMergeShowCommand(), "12"), "Merge: 12")
	snapshot := executePersonMergeCLI(t, newPersonMergeShowCommand(), "12", "--snapshot", "--json")
	assertions.JSONEq(`{"persons":[{"id":7}],"rows":{"person_names":[1]}}`,
		string(extractPersonMergeSnapshot(t, snapshot)))
	assertions.Contains(
		executePersonMergeCLI(t, newPersonMergeShowCommand(), "12", "--snapshot"),
		`Snapshot: {"persons":[{"id":7}],"rows":{"person_names":[1]}}`)
	executePersonMergeCLI(t, newPersonMergeCandidateCommand(), "21",
		"--person-id", "7", "--revision", "4", "--decision", "rejected", "--json")
	candidateOutput := executePersonMergeCLI(t, newPersonMergeCandidateCommand(), "21",
		"--person-id", "7", "--revision", "4", "--decision", "rejected")
	assertions.Contains(candidateOutput, "State: rejected")
	assertions.Contains(candidateOutput, `Person ETag: "person-7-r5"`)

	for _, key := range []string{
		"POST /api/v1/people/7/merge", "POST /api/v1/people/7/split",
		"GET /api/v1/people/7/merges", "GET /api/v1/person-merges/12",
		"GET /api/v1/person-merges/12/snapshot",
		"POST /api/v1/person-merge-candidates/21/decision",
	} {
		want := 2
		if key == "POST /api/v1/people/7/split" {
			want = 3
		}
		assertions.Equal(want, requests[key], key)
	}
	for _, command := range []*cobra.Command{
		newPersonMergeCommand(), newPersonSplitCommand(), newPersonMergeCandidateCommand(),
	} {
		assertions.Nil(command.Flags().Lookup("actor"))
	}
	assertions.Nil(newPersonMergeCandidateCommand().Flags().Lookup("idempotency-key"))
}

func extractPersonMergeSnapshot(t *testing.T, payload string) json.RawMessage {
	t.Helper()
	var decoded struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	return decoded.Snapshot
}

func TestPersonMergeCommandUsesExistingLocalDaemon(t *testing.T) {
	requests := 0
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService, Version: Version,
	}))
	mux.HandleFunc("/api/v1/people/7/merges", func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"merges":[]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	runtime := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(dataDir).Write(runtime.Record)
	require.NoError(t, err)
	stubStartServeBackgroundProcess(t,
		func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
			require.FailNow(t, "existing local daemon must not trigger autostart")
			return nil, errors.New("unreachable")
		})

	output := executePersonMergeCLI(t, newPersonMergeHistoryCommand(), "7", "--json")
	assert.JSONEq(t, `[]`, output)
	assert.Equal(t, 1, requests)
}

func TestPersonMergeCLIValidationHappensBeforeOpeningStore(t *testing.T) {
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	tests := []struct {
		name    string
		command *cobra.Command
		args    []string
		want    string
	}{
		{
			name: "merge revisions", command: newPersonMergeCommand(),
			args: []string{"1", "2", "--survivor-revision", "0",
				"--absorbed-revision", "1", "--idempotency-key", "merge-key"},
			want: "survivor revision must be a positive integer",
		},
		{
			name: "candidate decision", command: newPersonMergeCandidateCommand(),
			args: []string{"1", "--person-id", "1", "--revision", "1",
				"--decision", "maybe"},
			want: "decision must be accepted or rejected",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.command.SetArgs(test.args)
			err := test.command.Execute()
			require.ErrorContains(t, err, test.want)
		})
	}
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "invalid commands must not initialize the archive")
}
