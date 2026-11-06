package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const personFactCatalogResponse = `{
	"version":"v1","fingerprint":"catalog-fingerprint","targets":[{
		"kind":"attribute","key":"target-key","revision":"revision-1",
		"universal_id":"target-key","slug":"primary_channel",
		"description":"Preferred communication channel","value_type":"text",
		"cardinality":"single","choices":[],"fields":[],"sensitive":false
	}]}`

const personFactTestRevision = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const personFactEmploymentTarget = "employment:system:employment:" + personFactTestRevision

func TestStoreAPIAdapterServesPersonFactRoutes(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "person-facts-adapter@example.test", "Person Facts Adapter")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
	for _, path := range []string{
		"/api/v1/person-fact-targets",
		fmt.Sprintf("/api/v1/people/%d/fact-pins", person.ID),
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)

		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		requirements.Equal("no-store", response.Header().Get("Cache-Control"))
	}
}

func TestPersonFactsGeneratedPinsResponseDecodesBadPersonID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("/api/v1/people/7/fact-pins", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"error":"bad_request","message":"invalid person ID"}`))
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	client, _, err := OpenHTTPStore(testCtx)
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	generatedClient, err := client.GeneratedClient()
	requirements.NoError(err)
	response, err := generatedClient.ListPersonFactPinsWithResponse(testCtx,
		&generated.ListPersonFactPinsRequestOptions{
			PathParams: &generated.ListPersonFactPinsPath{ID: 7},
		})
	requirements.Error(err)
	requirements.NotNil(response)
	requirements.NotNil(response.JSON400)
	requirements.Equal("bad_request", response.JSON400.ErrorData)
}

func TestPersonFactsCatalogUsesGeneratedClientPathAndRendersJSONAndTable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/api/v1/person-fact-targets", r.URL.Path)
		queries = append(queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(personFactCatalogResponse))
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	table, err := runPersonFactsCommand(testCtx, t, "catalog")
	requirements.NoError(err)
	assertions.Contains(table, "KIND")
	assertions.Contains(table, "primary_channel")

	jsonOutput, err := runPersonFactsCommand(testCtx, t, "catalog", "--include-sensitive", "--json")
	requirements.NoError(err)
	assertions.JSONEq(personFactCatalogResponse, jsonOutput)
	requirements.Len(queries, 2)
	assertions.False(queries[0].Has("include_sensitive"), "catalog defaults to non-sensitive")
	assertions.Equal("true", queries[1].Get("include_sensitive"))
}

func TestPersonFactsHistoryCommandsUseExactGeneratedPathsQueriesAndTables(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	requests := make(map[string]url.Values)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		requests[r.URL.Path] = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/people/7/fact-evidence":
			_, _ = w.Write([]byte(`{"evidence":[{
				"id":9,"evidence_key":"evidence-key","source_class":"public",
				"directness":"direct-self","authority":"authoritative",
				"source_ref":"message:42","source_url":"https://example.test/evidence",
				"subject_person_id":7,"subject_ref":"synthetic-person",
				"excerpt":"Synthetic evidence","content_sha256":"digest",
				"source_version":"source-v1","event_time":"2026-08-22T10:00:00Z",
				"recorded_time":"2026-08-22T11:00:00Z","identity_score":990,
				"supported":true,"created_at":"2026-08-22T12:00:00Z"
			}]}`))
		case "/api/v1/people/7/fact-claims":
			_, _ = w.Write([]byte(`{"claims":[{
				"id":8,"generation_id":6,"claim_key":"claim-key",
				"program_id":"fixture-program","program_version":"v1",
				"program_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"target":{"kind":"attribute","key":"target-key","revision":"` + personFactTestRevision + `"},
				"relation":"support","submitted_value":"chat",
				"evidence_ids":[9],"origin":"extraction",
				"confidence":{"reported_score":900},"created_at":"2026-08-22T12:00:00Z"
			}]}`))
		case "/api/v1/people/7/fact-decisions":
			_, _ = w.Write([]byte(`{"decisions":[{
				"id":7,"resolution_id":5,"decision_key":"decision-key",
				"claim_key":"claim-key","action":"applied","reason":"applied-projection",
				"score":{"source_class":1,"directness":2,"authority":3,"confidence":4,
					"freshness":5,"corroboration":6,"total":21},
				"projection":{"kind":"person_attribute_value","row_id":11},
				"created_at":"2026-08-22T12:00:00Z"
			}]}`))
		case "/api/v1/people/7/fact-pins":
			_, _ = w.Write([]byte(`{"pins":[{
				"target":{"kind":"employment","key":"system:employment","revision":"` + personFactTestRevision + `"},
				"pinned":true,"actor":"api","event_id":4
			}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	for _, test := range []struct {
		command string
		header  string
		path    string
	}{
		{command: "evidence", header: "EVIDENCE KEY", path: "/api/v1/people/7/fact-evidence"},
		{command: "claims", header: "CLAIM KEY", path: "/api/v1/people/7/fact-claims"},
		{command: "decisions", header: "ACTION", path: "/api/v1/people/7/fact-decisions"},
	} {
		output, err := runPersonFactsCommand(testCtx, t, test.command, "7",
			"--target", personFactEmploymentTarget, "--limit", "4", "--offset", "2")
		requirements.NoError(err)
		assertions.Contains(output, test.header)
		query := requests[test.path]
		assertions.Equal(personFactEmploymentTarget, query.Get("target"))
		assertions.Equal("4", query.Get("limit"))
		assertions.Equal("2", query.Get("offset"))
	}

	pins, err := runPersonFactsCommand(testCtx, t, "pins", "7")
	requirements.NoError(err)
	assertions.Contains(pins, "PINNED")
	assertions.Contains(pins, "true")
	assertions.Contains(pins, personFactEmploymentTarget)

	jsonOutput, err := runPersonFactsCommand(testCtx, t, "claims", "7", "--json")
	requirements.NoError(err)
	assertions.JSONEq(`[{
		"id":8,"generation_id":6,"claim_key":"claim-key",
		"program_id":"fixture-program","program_version":"v1",
		"program_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"target":{"kind":"attribute","key":"target-key","revision":"`+personFactTestRevision+`"},
		"relation":"support","submitted_value":"chat","evidence_ids":[9],
		"origin":"extraction","confidence":{"reported_score":900},
		"created_at":"2026-08-22T12:00:00Z"
	}]`, jsonOutput)
}

func TestPersonFactsClaimsTableRetainsMalformedPersistedTargets(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/api/v1/people/7/fact-claims", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"claims":[
			{
				"id":8,"generation_id":6,"claim_key":"legacy-claim",
				"program_id":"fixture-program","program_version":"v1",
				"program_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"target":{"kind":"candidate","key":"legacy:key","revision":"legacy-revision"},
				"relation":"support","submitted_value":"legacy",
				"evidence_ids":[],"origin":"extraction",
				"confidence":{"reported_score":400},"created_at":"2026-08-22T12:00:00Z"
			},
			{
				"id":9,"generation_id":6,"claim_key":"valid-claim",
				"program_id":"fixture-program","program_version":"v1",
				"program_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"target":{"kind":"attribute","key":"target-key","revision":"` + personFactTestRevision + `"},
				"relation":"support","submitted_value":"valid",
				"evidence_ids":[],"origin":"extraction",
				"confidence":{"reported_score":900},"created_at":"2026-08-22T12:01:00Z"
			}
		]}`))
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	output, err := runPersonFactsCommand(testCtx, t, "claims", "7")
	requirements.NoError(err)
	assertions.Contains(output, "candidate:legacy:key:legacy-revision")
	assertions.Contains(output, "legacy-claim")
	assertions.Contains(output, "valid-claim")
}

func TestPersonFactsEvidenceStatusForwardsFalseFilterAndRendersNewestFirst(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/api/v1/people/7/fact-evidence-status-events", r.URL.Path)
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[
			{"id":12,"generation_id":4,"evidence_key":"evidence-key","source_version":"source-v1",
			 "supported":true,"reason":"source-reimported","created_at":"2026-08-22T12:02:00Z"},
			{"id":11,"generation_id":3,"evidence_key":"evidence-key","source_version":"source-v1",
			 "supported":false,"reason":"source-deleted","created_at":"2026-08-22T12:01:00Z"}
		]}`))
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	output, err := runPersonFactsCommand(testCtx, t, "evidence-status", "7",
		"--evidence-key", "evidence-key", "--supported=false", "--limit", "5", "--offset", "1")
	requirements.NoError(err)
	assertions.Contains(output, "SUPPORTED")
	assertions.Less(outputIndex(output, "source-reimported"), outputIndex(output, "source-deleted"),
		"server newest-first order is preserved")
	assertions.Equal("evidence-key", query.Get("evidence_key"))
	assertions.Equal("false", query.Get("supported"))
	assertions.Equal("5", query.Get("limit"))
	assertions.Equal("1", query.Get("offset"))
}

func TestPersonFactsPinAndUnpinSendExactBodyWithoutActorAndRenderProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var bodies []map[string]any
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPut, r.Method)
		paths = append(paths, r.URL.EscapedPath())
		var body map[string]any
		assertions.NoError(json.NewDecoder(r.Body).Decode(&body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"state":{"target":{"kind":"attribute","key":"target key","revision":"` + personFactTestRevision + `"},
				"pinned":false,"actor":"api","event_id":4},
			"resolutions":[{"id":5,"target":{"kind":"attribute","key":"target key","revision":"` + personFactTestRevision + `"},
				"resolver_version":"v1","input_fingerprint":"fingerprint",
				"resolved_at":"2026-08-22T12:00:00Z","decisions":[],
				"projections":[{"kind":"person_attribute_value","row_id":11}]}],
			"projections":[{"kind":"person_attribute_value","row_id":11}]
		}`))
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	for _, test := range []struct {
		command string
		pinned  bool
	}{
		{command: "pin", pinned: true},
		{command: "unpin", pinned: false},
	} {
		output, err := runPersonFactsCommand(testCtx, t, test.command, "7", "attribute", "target key")
		requirements.NoError(err)
		assertions.Contains(output, "person_attribute_value:11")
	}
	requirements.Len(bodies, 2)
	assertions.Equal(map[string]any{"pinned": true}, bodies[0])
	assertions.Equal(map[string]any{"pinned": false}, bodies[1])
	assertions.Equal([]string{
		"/api/v1/people/7/fact-pins/attribute/target%20key",
		"/api/v1/people/7/fact-pins/attribute/target%20key",
	}, paths)
}

func TestPersonFactsCommandsRejectInvalidInputsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	for _, args := range [][]string{
		{"evidence", "0"},
		{"claims", "7", "--limit", "201"},
		{"decisions", "7", "--offset", "-1"},
		{"evidence", "7", "--target", "not-a-target"},
		{"evidence", "7", "--target", "employment:system:employment:revision-1"},
		{"pin", "7", "candidate", "target-key"},
		{"unpin", "7", "attribute", " "},
	} {
		_, err := runPersonFactsCommand(testCtx, t, args...)
		require.Error(t, err, args)
	}
	assert.Zero(t, requests.Load())
}

func runPersonFactsCommand(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newPersonFactsCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	command.SetContext(ctx)
	err := command.Execute()
	return output.String(), err
}

func outputIndex(output, value string) int {
	return strings.Index(output, value)
}

func TestPersonFactsContestedDisplaysClaimsAndCitations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const sensitiveContests = `[{
		"target":{"kind":"attribute","key":"religion-key","slug":"religion","revision":"revision-2","sensitive":true},
		"resolution_revision":"revision-1","resolved_at":"2026-08-22T12:00:00Z",
		"current":[],"claims":[{
			"claim_key":"claim-buddhism","value":"\"Buddhism\"","relation":"support",
			"decision":{"reason":"competing-tie"},
			"evidence":[{"evidence_key":"evidence-buddhism","source_ref":"message:42",
				"source_url":"https://example.test/buddhism",
				"event_time":"2026-08-22T10:00:00Z","recorded_time":"2026-08-22T11:00:00Z"}]
		},{
			"claim_key":"claim-hinduism","value":"\"Hinduism\"","relation":"support",
			"decision":{"reason":"competing-tie"},"evidence":[]
		},{
			"claim_key":"claim-contradiction","value":"\"Buddhism\"","relation":"contradict",
			"decision":{"reason":"explicit-contradiction"},"evidence":[]
		}]
	}]`
	const employmentContests = `[{
		"target":{"kind":"employment","key":"system:employment","slug":"employment","revision":"revision-1","sensitive":false},
		"resolution_revision":"revision-1","resolved_at":"2026-08-22T12:00:00Z",
		"current":[],"claims":[]
	}]`
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		queries = append(queries, r.URL.Query())
		contests := `[]`
		switch r.URL.Path {
		case "/api/v1/people/7/fact-contests":
			if r.URL.Query().Get("include_sensitive") == "true" {
				contests = sensitiveContests
			}
		case "/api/v1/people/8/fact-contests":
		case "/api/v1/people/9/fact-contests":
			contests = employmentContests
		default:
			assertions.Fail("unexpected contested facts path", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintf(w, `{"contests":%s}`, contests)
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	defaultOutput, err := runPersonFactsCommand(ctx, t, "contested", "7")
	requirements.NoError(err)
	assertions.Equal("Sensitive fields are hidden; use --include-sensitive to include them.\nNo contested facts.\n", defaultOutput)

	output, err := runPersonFactsCommand(ctx, t, "contested", "7", "--include-sensitive")
	requirements.NoError(err)
	assertions.NotContains(output, "Sensitive fields are hidden")
	assertions.Contains(output, "attribute/religion (resolved 2026-08-22T12:00:00Z)")
	assertions.Contains(output, "Claim: \"Buddhism\" (supports; too close to choose)")
	assertions.Contains(output, "Claim: \"Hinduism\" (supports; too close to choose)")
	assertions.Contains(output, "Claim: \"Buddhism\" (contradicts)\n")
	for _, expected := range []string{"https://example.test/buddhism", "Definition changed since resolution"} {
		assertions.Contains(output, expected)
	}

	jsonOutput, err := runPersonFactsCommand(ctx, t, "contested", "7", "--include-sensitive", "--json")
	requirements.NoError(err)
	var contests []generated.PersonFactContest
	requirements.NoError(json.Unmarshal([]byte(jsonOutput), &contests))
	requirements.Len(contests, 1)
	assertions.Equal("religion", contests[0].Target.Slug)
	requirements.Len(contests[0].Claims, 3)
	assertions.Equal(`"Buddhism"`, contests[0].Claims[0].Value)
	assertions.Equal(`"Hinduism"`, contests[0].Claims[1].Value)
	requirements.Len(contests[0].Claims[0].Evidence, 1)
	assertions.Equal(new("https://example.test/buddhism"), contests[0].Claims[0].Evidence[0].SourceURL)

	empty, err := runPersonFactsCommand(ctx, t, "contested", "8", "--include-sensitive")
	requirements.NoError(err)
	assertions.Equal("No contested facts.\n", empty)

	command := newPersonFactsCommand()
	command.SetOut(failingSearchWriter{})
	command.SetErr(io.Discard)
	command.SetArgs([]string{"contested", "7", "--include-sensitive"})
	command.SetContext(ctx)
	requirements.ErrorIs(command.Execute(), io.ErrClosedPipe)

	output, err = runPersonFactsCommand(ctx, t, "contested", "9")
	requirements.NoError(err)
	assertions.Contains(output, "employment (resolved 2026-08-22T12:00:00Z)")
	assertions.NotContains(output, "employment/employment")

	requirements.Len(queries, 6)
	assertions.False(queries[0].Has("include_sensitive"), "contested defaults to non-sensitive")
	for _, query := range queries[1:5] {
		assertions.Equal("true", query.Get("include_sensitive"))
	}
	assertions.False(queries[5].Has("include_sensitive"))
}
