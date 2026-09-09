package daemonclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const savedViewFixtureJSON = `{
	"id":7,"name":"Invoices","description":"Reusable invoice search",
	"canonical_state":{
		"query":"quarterly invoice","search_mode":"hybrid",
		"filters":[{"field":"source_id","operator":"in","values":["3"]}],
		"presentation":"timeline","sort":[{"field":"occurred_at","direction":"desc"}],
		"columns":["kind","title","time"]
	},
	"schema_version":1,"revision":2,
	"created_at":"2026-08-18T08:00:00Z","updated_at":"2026-08-18T09:00:00Z"
}`

const savedViewRunFixtureJSON = `{
	"saved_view":` + savedViewFixtureJSON + `,
	"result_kind":"entries",
	"rows":[{
		"key":"message:42","kind":"email","anchor_message_id":42,
		"occurred_at":"2026-08-18T07:00:00Z","match":{"semantic_score":0.91},
		"source_id":3,"source_type":"gmail","source_identifier":"archive@example.com",
		"message_type":"email","conversation_type":"thread","title":"Quarterly invoice",
		"preview":"Invoice details","matched_sender_identities":["archive@example.com"],
		"matched_recipient_identities":["billing@example.com"],"message_count":1,
		"has_attachments":true,"attachment_count":1,"attachment_size":2048,
		"deleted_from_source":false
	}],
	"total_count":1,"next_cursor":"page-2","cache_revision":"cache-9",
	"search_provenance":{"lexical_index_revision":"fts-4","vector_generation":12},
	"candidate_snapshot_id":"snap-1","candidate_pool_saturated":true,
	"search_deletion_scope":"active"
}`

func TestRunSavedViewDelegatesExecutionToTheDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var body generated.RunSavedViewRequest
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/saved-views/7/run" {
			http.NotFound(w, r)
			return
		}
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		_, _ = w.Write([]byte(savedViewRunFixtureJSON))
	}))
	t.Cleanup(server.Close)

	page, err := newSavedViewTestClient(t, server).RunSavedView(t.Context(), 7, 25, "page-1")
	requirements.NoError(err)
	assertions.Equal(1, requests, "the daemon owns the view lookup and the Explore translation")
	requirements.NotNil(body.Limit)
	assertions.Equal(int64(25), *body.Limit)
	requirements.NotNil(body.Cursor)
	assertions.Equal("page-1", *body.Cursor)

	assertions.Equal(int64(7), page.View.ID)
	assertions.JSONEq(`{
		"query":"quarterly invoice","search_mode":"hybrid",
		"filters":[{"field":"source_id","operator":"in","values":["3"]}],
		"presentation":"timeline","sort":[{"field":"occurred_at","direction":"desc"}],
		"columns":["kind","title","time"]
	}`, string(page.View.CanonicalState))
	assertions.Equal(savedview.ResultEntries, page.ResultKind)
	requirements.Len(page.Rows, 1)
	assertions.Equal("Quarterly invoice", page.Rows[0].Title)
	assertions.Equal(1, page.Returned())
	requirements.NotNil(page.TotalCount)
	assertions.Equal(int64(1), *page.TotalCount)
	assertions.Equal("page-2", page.NextCursor)
	assertions.Equal("cache-9", page.CacheRevision)
	assertions.Equal("fts-4", page.SearchProvenance.LexicalIndexRevision)
	assertions.Equal("snap-1", page.CandidateSnapshotID)
	assertions.True(page.CandidatePoolSaturated)
	assertions.Equal("active", page.SearchDeletionScope)
}

func TestRunSavedViewDecodesGroupAndFilePages(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind savedview.ResultKind
		want func(t *testing.T, page *savedview.RunPage)
	}{
		{
			name: "groups",
			body: `{"saved_view":` + savedViewFixtureJSON + `,"result_kind":"groups",
				"groups":[{"key":"example.com","label":"example.com","count":4,"estimated_bytes":512,"latest_at":"2026-08-18T07:00:00Z"}],
				"total_count":1,"cache_revision":"cache-9","search_provenance":{}}`,
			kind: savedview.ResultGroups,
			want: func(t *testing.T, page *savedview.RunPage) {
				t.Helper()
				require.Len(t, page.Groups, 1)
				assert.Equal(t, "example.com", page.Groups[0].Key)
				assert.Equal(t, int64(4), page.Groups[0].Count)
				assert.Empty(t, page.Rows)
			},
		},
		{
			name: "files",
			body: `{"saved_view":` + savedViewFixtureJSON + `,"result_kind":"files",
				"files":[{"id":9,"key":"attachment:9","entry_key":"message:42","message_id":42,"conversation_id":1,
				"occurred_at":"2026-08-18T07:00:00Z","source_id":3,"source_identifier":"archive@example.com",
				"title":"Quarterly invoice","filename":"invoice.pdf","mime_type":"application/pdf","size":2048}],
				"total_count":1,"cache_revision":"cache-9","search_provenance":{}}`,
			kind: savedview.ResultFiles,
			want: func(t *testing.T, page *savedview.RunPage) {
				t.Helper()
				require.Len(t, page.Files, 1)
				assert.Equal(t, "invoice.pdf", page.Files[0].Filename)
				assert.Empty(t, page.Rows)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)

			page, err := newSavedViewTestClient(t, server).RunSavedView(t.Context(), 7, 20, "")
			require.NoError(t, err)
			assert.Equal(t, tc.kind, page.ResultKind)
			assert.Equal(t, 1, page.Returned())
			tc.want(t, page)
		})
	}
}

func TestRunSavedViewMapsDaemonErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{
			name: "vector capability", status: http.StatusServiceUnavailable,
			body:    `{"error":"vector_not_enabled","message":"Vector search is not configured"}`,
			wantErr: vector.ErrNotEnabled,
		},
		{
			name: "missing view", status: http.StatusNotFound,
			body:    `{"error":"saved_view_not_found","message":"Saved View not found"}`,
			wantErr: store.ErrSavedViewNotFound,
		},
		{
			name: "non-executable definition", status: http.StatusBadRequest,
			body:    `{"error":"invalid_saved_view","message":"Saved View 7 uses schema version 2"}`,
			wantErr: store.ErrSavedViewInvalidState,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)

			_, err := newSavedViewTestClient(t, server).RunSavedView(t.Context(), 7, 20, "")
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestSavedViewManagementUsesAPIRevisionContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var createBody map[string]any
	var patchBody map[string]any
	var patchMatch string
	var deleteMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/saved-views":
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&createBody)) {
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(savedViewFixtureJSON))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/saved-views/7":
			patchMatch = r.Header.Get("If-Match")
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&patchBody)) {
				return
			}
			_, _ = w.Write([]byte(`{
				"id":7,"name":"Receipts","canonical_state":{"query":"receipt","presentation":"table"},
				"schema_version":1,"revision":3,
				"created_at":"2026-08-18T08:00:00Z","updated_at":"2026-08-18T10:00:00Z"
			}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/saved-views/7":
			deleteMatch = r.Header.Get("If-Match")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newSavedViewTestClient(t, server)
	description := "Reusable invoice search"

	created, err := client.CreateSavedView(t.Context(), store.SavedViewInput{
		Name: "Invoices", Description: &description,
		CanonicalState: json.RawMessage(`{"query":"quarterly invoice","search_mode":"hybrid"}`),
		SchemaVersion:  1,
	})
	requirements.NoError(err)
	assertions.Equal(int64(7), created.ID)
	assertions.Equal("Invoices", createBody["name"])
	createState, ok := createBody["canonical_state"].(map[string]any)
	requirements.True(ok, "canonical_state: %#v", createBody["canonical_state"])
	assertions.Equal("hybrid", createState["search_mode"])

	name := "Receipts"
	emptyDescription := ""
	state := store.SavedViewStateEnvelope{Query: "receipt", Presentation: "table"}
	updated, err := client.UpdateSavedView(t.Context(), 7, 2, savedview.Patch{
		Name: &name, Description: &emptyDescription, CanonicalState: &state,
	})
	requirements.NoError(err)
	assertions.Equal(int64(3), updated.Revision)
	assertions.Equal(`"saved-view-7-r2"`, patchMatch)
	assertions.Equal("Receipts", patchBody["name"])
	assertions.Empty(patchBody["description"])
	patchState, ok := patchBody["canonical_state"].(map[string]any)
	requirements.True(ok, "canonical_state: %#v", patchBody["canonical_state"])
	assertions.Equal("receipt", patchState["query"])
	assertions.NotContains(patchBody, "schema_version")

	requirements.NoError(client.DeleteSavedView(t.Context(), 7, 3))
	assertions.Equal(`"saved-view-7-r3"`, deleteMatch)
}

func TestSavedViewManagementMapsDomainErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{
			"error":"saved_view_revision_conflict",
			"message":"Saved View revision does not match"
		}`))
	}))
	t.Cleanup(server.Close)

	name := "Stale"
	_, err := newSavedViewTestClient(t, server).UpdateSavedView(
		t.Context(), 7, 1, savedview.Patch{Name: &name},
	)
	require.ErrorIs(t, err, store.ErrSavedViewRevisionConflict)
}

func TestCreateSavedViewRejectsTransientStateBeforeRequest(t *testing.T) {
	requirements := require.New(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client := newSavedViewTestClient(t, server)

	_, err := client.CreateSavedView(t.Context(), store.SavedViewInput{
		Name: "Invalid", CanonicalState: json.RawMessage(`{"selection":[1]}`), SchemaVersion: 1,
	})
	requirements.ErrorIs(err, store.ErrSavedViewInvalidState)

	// Decoding into the typed request body would turn an explicit null into an
	// absent field, quietly storing a different definition than the store
	// would have accepted.
	_, err = client.CreateSavedView(t.Context(), store.SavedViewInput{
		Name: "Null filters", CanonicalState: json.RawMessage(`{"filters":null}`), SchemaVersion: 1,
	})
	requirements.ErrorIs(err, store.ErrSavedViewInvalidState)
	requirements.ErrorContains(err, "filters must not be null")

	assert.Zero(t, calls)
}

func newSavedViewTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
		Context: t.Context(), RequestMode: RequestModeCLI,
	})
	require.NoError(t, err)
	return client
}
