package cmd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cmd "go.kenn.io/msgvault/cmd/msgvault/cmd"
)

// These wire fixtures exercise the real Cobra command, HTTP resolver, generated
// client, and renderer. A synthetic daemon keeps request paging deterministic.
func tierCLI(t *testing.T, handler http.HandlerFunc, args ...string) (string, string, error) {
	t.Helper()
	return tierCLIForVersion(t, "3.24.0", handler, args...)
}

func tierCLIForVersion(t *testing.T, version string, handler http.HandlerFunc, args ...string) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": version}))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	command := cmd.PersonTiersCommandForTest(t.Context(), server.URL)
	var output, stderr bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&stderr)
	command.SetIn(strings.NewReader("yes\n"))
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), stderr.String(), err
}

const tierPage = `{"people":[{"person_id":7,"name":"Synthetic Example","signals":{"outbound_short":8},"prior":{"tier":3},"manual":{},"readiness":{"ready":false,"reasons":["activity checkpoint missing"]},"decision":{"tier":3,"computed_tier":2,"rule":"tier_2","demotion_due":"2026-10-31","reason":"direct interaction","deferred_reason":"activity not ready","lanes":{"tracking":"created","briefs":"blocked_opt_out"}},"opt_outs":[]}],"as_of":"2026-10-17T12:00:00Z","readiness":{"ready":false,"reasons":["activity checkpoint missing"]},"counts":{"tier_2":1,"blocked_manual":1}}`

func TestPersonTiersPreviewForwardsFiltersAndRawJSON(t *testing.T) {
	raw := "  " + tierPage + "\n"
	output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/people-tiers/preview", r.URL.Path)
		assert.Equal(t, "2", r.URL.Query().Get("tier"))
		assert.Equal(t, "true", r.URL.Query().Get("changes_only"))
		assert.Equal(t, "17", r.URL.Query().Get("limit"))
		assert.Equal(t, "page/+ cursor", r.URL.Query().Get("cursor"))
		_, _ = w.Write([]byte(raw))
	}, "preview", "--tier", "2", "--changes-only", "--limit", "17", "--cursor", "page/+ cursor", "--json")
	require.NoError(t, err)
	assert.Equal(t, raw, output)
}

func TestPersonTiersPreviewShowsReadinessAndPlan(t *testing.T) {
	assert := assert.New(t)
	output, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tierPage)) }, "preview")
	require.NoError(t, err)
	for _, value := range []string{"Synthetic Example", "2026-10-17", "activity checkpoint missing", "tier_2", "tracking", "briefs", "opt-out", "manual", "2026-10-31"} {
		assert.Contains(output, value)
	}
	assert.Contains(output, "create tracking")
	assert.Contains(output, "blocked: opt-out briefs")
}

func TestPersonTiersApplyRefusesNonTTYWithoutYes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var writes atomic.Int32
	output, stderr, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_, _ = w.Write([]byte(tierPage))
	}, "apply")
	require.Error(err)
	require.ErrorContains(err, "--yes")
	assert.Empty(output)
	assert.Contains(stderr, "Synthetic Example")
	assert.Contains(stderr, "Apply scope: all people")
	assert.Zero(writes.Load())
}

func TestPersonTiersApplyDryRunOnlyPreviews(t *testing.T) {
	output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/people-tiers/preview", r.URL.Path)
		_, _ = w.Write([]byte(tierPage))
	}, "apply", "--dry-run", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, tierPage, output)
}

func TestPersonTiersApplyCarriesRunAndCursorAndBoundsMax(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int32
	output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/people-tiers/apply", r.URL.Path)
		var body map[string]any
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch calls.Add(1) {
		case 1:
			assert.InDelta(2, body["limit"], 0)
			assert.Empty(body["run_id"])
			assert.Empty(body["cursor"])
			_, _ = w.Write([]byte(`{"people":[{"person_id":7},{"person_id":8}],"run_id":"one-run","next_cursor":"next-one","counts":{"created":4,"blocked_manual":1,"deferred":2}}`))
		case 2:
			assert.InDelta(1, body["limit"], 0)
			assert.Equal("one-run", body["run_id"])
			assert.Equal("next-one", body["cursor"])
			_, _ = w.Write([]byte(`{"people":[{"person_id":9}],"run_id":"one-run","next_cursor":"more","counts":{"removed":1}}`))
		default:
			assert.Fail("apply exceeded --max")
			w.WriteHeader(http.StatusBadRequest)
		}
	}, "apply", "--yes", "--limit", "2", "--max", "3")
	require.NoError(err)
	assert.Equal(int32(2), calls.Load())
	assert.Contains(output, "created=4")
	assert.Contains(output, "evaluated=3")
	assert.Contains(output, "removed")
	assert.Contains(output, "blocked")
	assert.Contains(strings.ToLower(output), "lane actions")
	assert.Contains(output, "deferred")
}

func TestPersonTiersResetRequiresYesBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	_, _, err := tierCLI(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) }, "reset")
	require.Error(t, err)
	require.ErrorContains(t, err, "--yes")
	assert.Zero(t, requests.Load())
}

func TestPersonTiersResetConfirmsAndForwardsJSON(t *testing.T) {
	raw := "{\"cleared\":4,\"deferred\":[]}\n"
	output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/people-tiers/reset", r.URL.Path)
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, true, body["confirm"])
		_, _ = w.Write([]byte(raw))
	}, "reset", "--yes", "--json")
	require.NoError(t, err)
	assert.Equal(t, raw, output)
}

func TestPersonTiersReadsHistoryAndCandidatePagination(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		path  string
		query map[string]string
		body  string
	}{
		{[]string{"history", "--person", "7", "--limit", "12", "--offset", "5", "--json"}, "/api/v1/people-tiers/history", map[string]string{"person_id": "7", "limit": "12", "offset": "5"}, `{"events":[]}`},
		{[]string{"candidates", "--limit", "21", "--cursor", "candidate/+", "--json"}, "/api/v1/people-tiers/candidates", map[string]string{"limit": "21", "cursor": "candidate/+"}, `{"candidates":[],"next_cursor":"next"}`},
	} {
		t.Run(tc.args[0], func(t *testing.T) {
			output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, tc.path, r.URL.Path)
				for key, value := range tc.query {
					assert.Equal(t, value, r.URL.Query().Get(key))
				}
				_, _ = w.Write([]byte(tc.body))
			}, tc.args...)
			require.NoError(t, err)
			assert.Equal(t, tc.body+"\n", output)
		})
	}
}

func TestPersonTierOptOutAndOptInUseGeneratedRoute(t *testing.T) {
	for _, action := range []string{"opt-out", "opt-in"} {
		t.Run(action, func(t *testing.T) {
			args := []string{action, "7", "--lane", "enrichment"}
			if action == "opt-out" {
				args = append(args, "--reason", "pause automation")
			}
			output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/people/7/tier/opt-outs", r.URL.Path)
				var body map[string]any
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, "enrichment", body["lane"])
				assert.Equal(t, action == "opt-out", body["opted_out"])
				if action == "opt-out" {
					assert.Equal(t, "pause automation", body["reason"])
				}
				_, _ = w.Write([]byte(`{"person_id":7}`))
			}, args...)
			require.NoError(t, err)
			assert.Contains(t, output, "enrichment")
		})
	}
}

func TestPersonTiersRejectsInvalidArgumentsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"preview", "--tier", "4"}, {"preview", "--limit", "501"},
		{"apply", "--limit", "0", "--yes"}, {"apply", "--max", "-1", "--yes"},
		{"explain", "0"}, {"history", "--limit", "201"}, {"history", "--offset", "-1"},
		{"candidates", "--limit", "501"}, {"opt-out", "7", "--lane", "other"}, {"opt-in", "7"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var requests atomic.Int32
			_, _, err := tierCLI(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) }, args...)
			require.Error(t, err)
			assert.Zero(t, requests.Load())
		})
	}
}

const tierExplanation = `{"person_id":7,"name":"Synthetic\u001b[31m Example","signals":{"outbound_short":8,"outbound_long":15,"inbound_short":4,"inbound_long":9,"active_days_short":5,"active_days_long":9,"meeting_days_long":2,"channels_long":2,"excluded_list_events":3,"large_audience_inbound_events":4,"co_presence_events":5,"observed_events":40},"prior":{"tier":2,"candidate_tier":3,"candidate_since":"2026-10-01"},"manual":{},"readiness":{"ready":false,"reasons":["activity checkpoint missing"]},"decision":{"tier":2,"computed_tier":3,"rule":"tier_3","demotion_due":"2026-10-31","thresholds":[{"name":"outbound_short","observed":8,"required":6,"enabled":true,"met":true}]},"tracking":{"person_id":7,"tracked":true,"provenance":"manual","actor":"","tracked_at":"2026-10-17T12:00:00Z"},"briefs":null,"opt_outs":[{"id":1,"person_id":7,"lane":"enrichment","actor":"synthetic-actor","reason":"pause\u001b[31m automation","created_at":"2026-10-17T12:00:00Z"}],"linked_participants":1,"signal_note":"archived patterns","known_suppression":true,"suppression_note":"known suppression","as_of":"2026-10-17T12:00:00Z","enrichment_eligible":false,"enrichment_consent":true,"recent_events":[{"id":1,"person_id":7,"reason":"auto_promote","mode":"apply","to_tier":2,"actor":"synthetic-actor","created_at":"2026-10-16T12:00:00Z"}]}`

func TestPersonTiersExplainShowsEvidenceAndSanitizesTerminalText(t *testing.T) {
	output, _, err := tierCLI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/people/7/tier", r.URL.Path)
		_, _ = w.Write([]byte(tierExplanation))
	}, "explain", "7")
	require.NoError(t, err)
	for _, value := range []string{"Synthetic Example", "outbound_short", "8", "6", "excluded_list_events", "activity checkpoint missing", "manual", "unknown (legacy row)", "synthetic-actor", "pause automation", "2026-10-31", "auto_promote", "consent", "suppression"} {
		assert.Contains(t, output, value)
	}
	assert.NotContains(t, output, "\x1b")
}

func TestPersonTiersExplainJSONKeepsDaemonBody(t *testing.T) {
	output, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tierExplanation)) }, "explain", "7", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, tierExplanation, output)
}

func TestPersonTiersApplyJSONPreservesEveryPageBody(t *testing.T) {
	first := ` {"people":[{"person_id":7}],"run_id":"one-run","next_cursor":"next","counts":{"created":1}}`
	last := ` {"people":[{"person_id":8}],"run_id":"one-run","next_cursor":"","counts":{"removed":1}}`
	var calls atomic.Int32
	output, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(first))
			return
		}
		_, _ = w.Write([]byte(last))
	}, "apply", "--yes", "--json")
	require.NoError(t, err)
	assert.Equal(t, first+"\n"+last+"\n", output)
	assert.Equal(t, int32(2), calls.Load())
}

func TestPersonTiersApplyStopsOnInvalidContinuation(t *testing.T) {
	for _, tc := range []struct{ name, second string }{
		{"changed run", `{"people":[{"person_id":8}],"run_id":"changed","next_cursor":"more"}`},
		{"repeated cursor", `{"people":[{"person_id":8}],"run_id":"one-run","next_cursor":"same"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			_, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) {
				switch calls.Add(1) {
				case 1:
					_, _ = w.Write([]byte(`{"people":[{"person_id":7}],"run_id":"one-run","next_cursor":"same"}`))
				case 2:
					_, _ = w.Write([]byte(tc.second))
				default:
					w.WriteHeader(http.StatusBadRequest)
				}
			}, "apply", "--yes")
			require.Error(t, err)
			assert.Equal(t, int32(2), calls.Load())
		})
	}
}

func TestPersonTiersCandidatesNamesApproximationAndManualNextStep(t *testing.T) {
	output, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"participant_id":19,"canonical_id":"synthetic@example.test","basis":"participant_approximation","suggested_command":"msgvault person promote 19","outbound_short":8,"inbound_short":4,"active_days_short":5,"identity_match_candidate_id":23}],"next_cursor":"19"}`))
	}, "candidates")
	require.NoError(t, err)
	for _, value := range []string{"19", "participant_approximation", "synthetic@example.test", "msgvault person promote 19", "23"} {
		assert.Contains(t, output, value)
	}
	assert.Contains(t, output, "cursor")
}

func TestPersonTiersResetShowsDeferredManualRun(t *testing.T) {
	output, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cleared":4,"deferred":[{"person_id":7,"run_id":"manual-run"}]}`))
	}, "reset", "--yes")
	require.NoError(t, err)
	for _, value := range []string{"4", "7", "manual-run", "deferred"} {
		assert.Contains(t, output, value)
	}
}

func TestPersonTiersHistoryDisplaysEventsAndSanitizesText(t *testing.T) {
	output, _, err := tierCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"events":[{"id":21,"person_id":7,"from_tier":3,"to_tier":2,"reason":"promotion","mode":"apply","rule":"tier2_two_way","actor":"synthetic\u001b[31m-actor","run_id":"synthetic-run","created_at":"2026-10-17T12:00:00Z"}]}`))
	}, "history")
	require.NoError(t, err)
	for _, value := range []string{"21", "7", "promotion", "apply", "synthetic-actor", "synthetic-run", "2026-10-17"} {
		assert.Contains(t, output, value)
	}
	assert.NotContains(t, output, "\x1b")
}

func TestPersonTiersCommandsAreRegisteredOnPersonTree(t *testing.T) {
	for _, action := range []string{"preview", "explain", "apply", "reset", "history", "candidates", "opt-out", "opt-in"} {
		t.Run(action, func(t *testing.T) {
			command, _, err := cmd.PersonRootCommandForTest().Find([]string{"tiers", action})
			require.NoError(t, err)
			assert.Equal(t, action, command.Name())
		})
	}
}

func TestPersonTiersRejectsOlderDaemonBeforeNewRoutes(t *testing.T) {
	for _, args := range [][]string{
		{"preview"}, {"explain", "7"}, {"apply", "--yes"}, {"reset", "--yes"}, {"history"}, {"candidates"},
		{"opt-out", "7", "--lane", "enrichment"}, {"opt-in", "7", "--lane", "enrichment"},
	} {
		t.Run(args[0], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests atomic.Int32
			output, _, err := tierCLIForVersion(t, "3.23.0", func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNotFound) }, args...)
			require.Error(err)
			require.ErrorContains(err, "requires daemon API schema 3.24.0")
			require.ErrorContains(err, "upgrade")
			assert.Zero(requests.Load(), "unsupported tier routes must not be requested")
			assert.Empty(output)
		})
	}
}
