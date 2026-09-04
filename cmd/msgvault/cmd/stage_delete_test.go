package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newRegisteredStageDeleteTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	registered, _, err := rootCmd.Find([]string{"stage-delete"})
	require.NoError(t, err, "find registered stage-delete command")
	require.NotNil(t, registered, "registered stage-delete command")

	stageDeleteDryRun = false
	require.NoError(t, registered.Flags().Set("dry-run", "false"))
	require.NoError(t, registered.Flags().Set("source-id", "0"))
	registered.Flags().Lookup("source-id").Changed = false
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(registered)
	return root
}

func TestStageDeleteCommand(t *testing.T) {
	tests := []struct {
		name       string
		query      []string
		flags      []string
		dryRun     bool
		status     int
		wantQuery  string
		wantSource int64
		wantOutput string
	}{
		{
			name:       "search_criteria",
			query:      []string{"from:alice@example.com", "older_than:1y"},
			status:     http.StatusCreated,
			wantQuery:  "from:alice@example.com older_than:1y",
			wantOutput: "Staged 3 message(s) for deletion in batch batch-191.\nReview with 'msgvault show-deletion batch-191', then execute with 'msgvault delete-staged batch-191'.\n",
		},
		{
			name:       "filter_only_criteria",
			query:      []string{"label:INBOX"},
			status:     http.StatusCreated,
			wantQuery:  "label:INBOX",
			wantOutput: "Staged 3 message(s) for deletion in batch batch-191.\nReview with 'msgvault show-deletion batch-191', then execute with 'msgvault delete-staged batch-191'.\n",
		},
		{
			name:       "dry_run",
			query:      []string{"subject:receipt"},
			dryRun:     true,
			status:     http.StatusOK,
			wantQuery:  "subject:receipt",
			wantOutput: "Dry run: 3 message(s) match the search; no deletion batch was created.\n",
		},
		{
			name:       "source_id",
			query:      []string{"from:alice@example.com"},
			flags:      []string{"--source-id", "42"},
			status:     http.StatusCreated,
			wantQuery:  "from:alice@example.com",
			wantSource: 42,
			wantOutput: "Staged 3 message(s) for deletion in batch batch-191.\nReview with 'msgvault show-deletion batch-191', then execute with 'msgvault delete-staged batch-191'.\n",
		},
		{
			name:       "list_id",
			query:      []string{"list:announce.example.org"},
			status:     http.StatusCreated,
			wantQuery:  "list:announce.example.org",
			wantOutput: "Staged 3 message(s) for deletion in batch batch-191.\nReview with 'msgvault show-deletion batch-191', then execute with 'msgvault delete-staged batch-191'.\n",
		},
		{
			name:       "list_id_alias",
			query:      []string{"list-id:announce.example.org"},
			status:     http.StatusCreated,
			wantQuery:  "list-id:announce.example.org",
			wantOutput: "Staged 3 message(s) for deletion in batch batch-191.\nReview with 'msgvault show-deletion batch-191', then execute with 'msgvault delete-staged batch-191'.\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wantSource []int64
			if tt.wantSource != 0 {
				wantSource = []int64{tt.wantSource}
			}
			server, routes := newStageDeleteTestServer(t, tt.wantQuery, tt.dryRun, tt.status, wantSource...)
			withStoreResolverConfig(t, &config.Config{
				Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
			})

			var stdout bytes.Buffer
			root := newRegisteredStageDeleteTestRoot(t)
			root.SetOut(&stdout)
			args := append([]string{"stage-delete"}, tt.query...)
			args = append(args, tt.flags...)
			args = append(args, boolFlag(tt.dryRun, "--dry-run")...)
			root.SetArgs(args)

			require.NoError(t, root.Execute(), tt.name)
			assert.Equal(t, tt.wantOutput, stdout.String())
			assert.Equal(t, []string{"/api/v1/cli/search", "/api/v1/explore", "/api/v1/explore/preflight", "/api/v1/deletions"}, *routes)
		})
	}

	t.Run("configured_remote_routing", func(t *testing.T) {
		server, routes := newStageDeleteTestServer(t, "from:bob@example.com", false, http.StatusCreated)
		withStoreResolverConfig(t, &config.Config{
			Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
		})

		var stdout bytes.Buffer
		root := newRegisteredStageDeleteTestRoot(t)
		root.SetOut(&stdout)
		root.SetArgs([]string{"stage-delete", "from:bob@example.com"})

		require.NoError(t, root.Execute())
		assert.Contains(t, stdout.String(), "batch-191")
		assert.Equal(t, []string{"/api/v1/cli/search", "/api/v1/explore", "/api/v1/explore/preflight", "/api/v1/deletions"}, *routes)
	})

	t.Run("invalid_and_empty", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			args []string
			want string
		}{
			{name: "invalid", args: []string{"before:not-a-date"}, want: "invalid value"},
			{name: "empty", args: []string{"   "}, want: "empty search query"},
			{name: "empty_from", args: []string{"from:"}, want: "empty address filter"},
			{name: "empty_to", args: []string{"to:"}, want: "empty address filter"},
			{name: "empty_cc", args: []string{"cc:"}, want: "empty address filter"},
			{name: "empty_bcc", args: []string{"bcc:"}, want: "empty address filter"},
			{name: "zero_source_id", args: []string{"subject:test", "--source-id", "0"}, want: "source ID must be positive"},
			{name: "negative_source_id", args: []string{"subject:test", "--source-id", "-1"}, want: "source ID must be positive"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					requests++
				}))
				defer server.Close()
				withStoreResolverConfig(t, &config.Config{
					Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
				})

				root := newRegisteredStageDeleteTestRoot(t)
				root.SetArgs(append([]string{"stage-delete"}, tt.args...))
				err := root.Execute()

				require.ErrorContains(t, err, tt.want)
				assert.Zero(t, requests, "invalid input must not make an HTTP request")
			})
		}
	})

	t.Run("cache_unavailable_reports_recovery", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/cli/search":
				writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{"results": []any{}})
			case "/api/v1/explore":
				writeStageDeleteJSON(t, w, http.StatusServiceUnavailable, map[string]any{
					"error":     "analytical_cache_unavailable",
					"message":   "The analytical cache is being prepared",
					"readiness": "building",
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		withStoreResolverConfig(t, &config.Config{
			Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
		})

		root := newRegisteredStageDeleteTestRoot(t)
		root.SetArgs([]string{"stage-delete", "subject:receipt"})
		err := root.Execute()

		require.ErrorContains(t, err, "The analytical cache is being prepared")
		require.ErrorContains(t, err, "msgvault build-cache")
	})

	t.Run("incomplete_search_index_blocks_staging", func(t *testing.T) {
		exploreRequests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/cli/search":
				writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{
					"results":     []any{},
					"index_state": "building",
				})
			case "/api/v1/explore":
				exploreRequests++
				http.Error(w, "unexpected Explore request", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		withStoreResolverConfig(t, &config.Config{
			Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
		})

		root := newRegisteredStageDeleteTestRoot(t)
		root.SetArgs([]string{"stage-delete", "subject:receipt"})
		err := root.Execute()

		require.ErrorContains(t, err, "could miss matching messages")
		assert.Zero(t, exploreRequests, "an incomplete search index must block staging before Explore")
	})

	t.Run("list_id_rejects_old_daemon_before_explore", func(t *testing.T) {
		for _, tt := range []struct {
			name  string
			query string
		}{
			{name: "list", query: "list:announce.example.org"},
			{name: "list_id", query: "list-id:announce.example.org"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				exploreRequests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/health":
						writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{
							"status":             "ok",
							"api_schema_version": "2.13.0",
						})
					case "/api/v1/explore":
						exploreRequests++
						http.Error(w, "unexpected Explore request", http.StatusInternalServerError)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				withStoreResolverConfig(t, &config.Config{
					Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
				})

				root := newRegisteredStageDeleteTestRoot(t)
				root.SetArgs([]string{"stage-delete", tt.query})
				err := root.Execute()

				require.ErrorContains(t, err, "List-ID filter requires daemon API schema 2.14.0 or newer")
				assert.Zero(t, exploreRequests, "an older daemon must reject List-ID queries before Explore")
			})
		}
	})

	t.Run("list_id_capability_probe_failures_before_explore", func(t *testing.T) {
		for _, tt := range []struct {
			name          string
			status        int
			body          string
			closeBefore   bool
			wantHealthReq int
		}{
			{name: "http_error", status: http.StatusInternalServerError, body: "health failed", wantHealthReq: 1},
			{name: "malformed_response", status: http.StatusOK, body: "{", wantHealthReq: 1},
			{name: "connection_failure", closeBefore: true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				exploreRequests := 0
				healthRequests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/health" {
						healthRequests++
						if tt.status == http.StatusOK {
							w.WriteHeader(tt.status)
							_, err := w.Write([]byte(tt.body))
							assert.NoError(t, err)
							return
						}
						http.Error(w, tt.body, tt.status)
						return
					}
					if r.URL.Path == "/api/v1/explore" {
						exploreRequests++
					}
					http.NotFound(w, r)
				}))
				if tt.closeBefore {
					server.Close()
				} else {
					defer server.Close()
				}
				withStoreResolverConfig(t, &config.Config{
					Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
				})

				root := newRegisteredStageDeleteTestRoot(t)
				root.SetArgs([]string{"stage-delete", "list:announce.example.org"})
				err := root.Execute()

				require.ErrorContains(t, err, "check daemon List-ID filter capability")
				assert.Equal(t, tt.wantHealthReq, healthRequests)
				assert.Zero(t, exploreRequests, "a failed capability probe must happen before Explore")
			})
		}
	})

	t.Run("newer_daemon_and_ordinary_query", func(t *testing.T) {
		t.Run("newer_daemon", func(t *testing.T) {
			server, routes, healthRequests := newStageDeleteTestServerWithSchema(t, "list:announce.example.org", false, http.StatusCreated, "2.15.0")
			withStoreResolverConfig(t, &config.Config{
				Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
			})

			root := newRegisteredStageDeleteTestRoot(t)
			root.SetArgs([]string{"stage-delete", "list:announce.example.org"})
			require.NoError(t, root.Execute())
			assert.Equal(t, []string{"/api/v1/cli/search", "/api/v1/explore", "/api/v1/explore/preflight", "/api/v1/deletions"}, *routes)
			assert.Equal(t, 2, *healthRequests,
				"the stage-delete gate and the search-index probe each verify List-ID capability")
		})

		t.Run("ordinary_query_skips_probe", func(t *testing.T) {
			server, routes, healthRequests := newStageDeleteTestServerWithSchema(t, "subject:test", false, http.StatusCreated, "2.15.0")
			withStoreResolverConfig(t, &config.Config{
				Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
			})

			root := newRegisteredStageDeleteTestRoot(t)
			root.SetArgs([]string{"stage-delete", "subject:test"})
			require.NoError(t, root.Execute())
			assert.Equal(t, []string{"/api/v1/cli/search", "/api/v1/explore", "/api/v1/explore/preflight", "/api/v1/deletions"}, *routes)
			assert.Zero(t, *healthRequests)
		})
	})
}

func boolFlag(enabled bool, flag string) []string {
	if enabled {
		return []string{flag}
	}
	return nil
}

func newStageDeleteTestServer(t *testing.T, wantQuery string, dryRun bool, stageStatus int, wantSourceID ...int64) (*httptest.Server, *[]string) {
	t.Helper()
	server, routes, _ := newStageDeleteTestServerWithSchema(t, wantQuery, dryRun, stageStatus, "2.14.0", wantSourceID...)
	return server, routes
}

func newStageDeleteTestServerWithSchema(t *testing.T, wantQuery string, dryRun bool, stageStatus int, schemaVersion string, wantSourceID ...int64) (*httptest.Server, *[]string, *int) {
	t.Helper()
	routes := make([]string, 0, 3)
	healthRequests := 0
	var expectedSourceID *int64
	if len(wantSourceID) > 0 {
		expectedSourceID = &wantSourceID[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		healthRequests++
		writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{
			"status":             "ok",
			"api_schema_version": schemaVersion,
		})
	})
	mux.HandleFunc("/api/v1/cli/search", func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, wantQuery, r.URL.Query().Get("q"))
		writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{"results": []any{}})
	})
	mux.HandleFunc("/api/v1/explore", func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		var request generated.ExploreHTTPRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "malformed explore request", http.StatusBadRequest)
			return
		}
		if !assert.NotNil(t, request.Query) || !assert.NotNil(t, request.SearchMode) || !assert.NotNil(t, request.Limit) {
			http.Error(w, "missing explore request fields", http.StatusBadRequest)
			return
		}
		assert.Equal(t, wantQuery, *request.Query)
		assert.Equal(t, generated.ExploreHTTPRequestSearchModeFullText, *request.SearchMode)
		assert.Equal(t, int64(1), *request.Limit)
		assertStageDeleteFilters(t, request.Filters, expectedSourceID)
		writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{
			"cache_revision":        "cache-191",
			"rows":                  []any{},
			"search_provenance":     map[string]any{"lexical_index_revision": "lex-191"},
			"candidate_snapshot_id": "snapshot-191",
		})
	})
	mux.HandleFunc("/api/v1/explore/preflight", func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		var request generated.ExplorePreflightRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "malformed preflight request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, generated.ExploreSelectionModeAllMatching, request.Selection.Mode)
		assert.Equal(t, "cache-191", request.Selection.CacheRevision)
		if !assert.NotNil(t, request.Selection.Predicate.Query) || !assert.NotNil(t, request.Selection.Predicate.SearchMode) {
			http.Error(w, "missing preflight request fields", http.StatusBadRequest)
			return
		}
		assert.Equal(t, wantQuery, *request.Selection.Predicate.Query)
		assert.Equal(t, generated.ExploreHTTPRequestSearchModeFullText, *request.Selection.Predicate.SearchMode)
		assertStageDeleteFilters(t, request.Selection.Predicate.Filters, expectedSourceID)
		if assert.NotNil(t, request.Selection.SearchProvenance.LexicalIndexRevision) {
			assert.Equal(t, "lex-191", *request.Selection.SearchProvenance.LexicalIndexRevision)
		}
		writeStageDeleteJSON(t, w, http.StatusOK, map[string]any{
			"cache_revision":      "cache-191",
			"count":               3,
			"operation_token":     "operation-191",
			"expires_at":          "2099-01-01T00:00:00Z",
			"search_provenance":   map[string]any{"lexical_index_revision": "lex-191"},
			"action_targets":      []any{},
			"unavailable_actions": []any{},
		})
	})
	mux.HandleFunc("/api/v1/deletions", func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		var request generated.StageDeletionRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "malformed stage deletion request", http.StatusBadRequest)
			return
		}
		if !assert.NotNil(t, request.Selection) || !assert.NotNil(t, request.OperationToken) ||
			!assert.NotNil(t, request.Description) || !assert.NotNil(t, request.DryRun) {
			http.Error(w, "missing stage deletion request fields", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "operation-191", *request.OperationToken)
		assert.Equal(t, "staged from CLI search", *request.Description)
		assert.Equal(t, dryRun, *request.DryRun)
		assert.Equal(t, generated.ExploreSelectionModeAllMatching, request.Selection.Mode)
		assertStageDeleteFilters(t, request.Selection.Predicate.Filters, expectedSourceID)
		response := map[string]any{
			"dry_run":       dryRun,
			"message_count": 3,
			"account":       "alice@example.com",
		}
		if !dryRun {
			response["id"] = "batch-191"
			response["status"] = "pending"
		}
		writeStageDeleteJSON(t, w, stageStatus, response)
	})
	mux.HandleFunc("/api/v1/cli/run", func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "stage-delete must not use the daemon CLI runner", r.URL.Path)
		http.Error(w, "unexpected CLI runner request", http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "unexpected stage-delete route", r.URL.Path)
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux), &routes, &healthRequests
}

func assertStageDeleteFilters(t *testing.T, filters []generated.ExploreFilter, wantSourceID *int64) {
	t.Helper()
	expected := []generated.ExploreFilter{{
		Dimension: generated.ExploreFilterDimensionDeletion,
		Values:    []string{"active"},
	}}
	if wantSourceID != nil {
		expected = append(expected, generated.ExploreFilter{
			Dimension: generated.ExploreFilterDimensionSource,
			Values:    []string{strconv.FormatInt(*wantSourceID, 10)},
		})
	}
	assert.Equal(t, expected, filters)
}

func writeStageDeleteJSON(t *testing.T, w http.ResponseWriter, status int, value map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	assert.NoError(t, json.NewEncoder(w).Encode(value))
}

func TestStageDeleteCommandRegistration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	command, _, err := rootCmd.Find([]string{"stage-delete"})
	require.NoError(err)
	require.NotNil(command)
	assert.Equal("stage-delete", command.Name())
	assert.Equal("stage-delete <query>", command.Use)
}
