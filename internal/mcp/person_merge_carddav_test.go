package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPPersonCardDAVCompletionToolGates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := ServeOptions{Engine: &querytest.MockEngine{}}
	old := toolsByName(t, rawListTools(t, base, true))
	assert.NotContains(old, ToolGetPersonMergeContext)
	assert.NotContains(old, ToolSyncCardDAV)

	backend := newIdentityReviewMCPBackend(t)
	base.PersonCardDAV = backend
	read := toolsByName(t, rawListTools(t, base, false))
	for _, name := range []string{ToolGetPersonMergeContext, ToolGetCardDAVPublication, ToolPreviewCardDAVPublication, ToolGetCardDAVSyncStatus} {
		require.Contains(read, name)
	}
	for _, name := range []string{ToolMergePerson, ToolApproveCardDAVPublication, ToolSyncCardDAV} {
		assert.NotContains(read, name)
	}
	generalWriteOnly := toolsByName(t, rawListTools(t, base, true))
	for _, name := range []string{ToolMergePerson, ToolApproveCardDAVPublication, ToolSyncCardDAV} {
		assert.NotContains(generalWriteOnly, name)
	}
	base.AllowProfileWrites = true
	profileWrites := toolsByName(t, rawListTools(t, base, true))
	for _, name := range []string{ToolMergePerson, ToolApproveCardDAVPublication, ToolSyncCardDAV} {
		assert.NotContains(profileWrites, name)
	}
	base.AllowPersonMerges = true
	mergeOnly := toolsByName(t, rawListTools(t, base, true))
	require.Contains(mergeOnly, ToolMergePerson)
	assert.NotContains(mergeOnly, ToolApproveCardDAVPublication)
	assert.NotContains(mergeOnly, ToolSyncCardDAV)
	base.AllowCardDAVWrites = true
	writable := toolsByName(t, rawListTools(t, base, true))
	for _, name := range []string{ToolMergePerson, ToolApproveCardDAVPublication, ToolSyncCardDAV} {
		require.Contains(writable, name)
	}
	assert.Equal(true, toolReadOnlyHint(t, writable[ToolGetPersonMergeContext]))
	assert.Equal(false, toolReadOnlyHint(t, writable[ToolMergePerson]))
	for _, name := range []string{ToolApproveCardDAVPublication, ToolSyncCardDAV} {
		annotations, ok := writable[name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(true, annotations["openWorldHint"])
	}
}

func TestMCPPersonCardDAVCompletionMergeRouteGuards(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	leftID, err := st.EnsureParticipantByIdentifier("email", "left@example.com", "Left")
	require.NoError(err)
	rightID, err := st.EnsureParticipantByIdentifier("email", "right@example.com", "Right")
	require.NoError(err)
	left, _, err := st.CreatePersonFromParticipant(leftID)
	require.NoError(err)
	right, _, err := st.CreatePersonFromParticipant(rightID)
	require.NoError(err)
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(srv.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PersonCardDAV: client, AllowPersonMerges: true}
	ids := map[string]any{"survivor_person_id": float64(left.ID), "absorbed_person_id": float64(right.ID)}
	contextResult := rawCallTool(t, opts, ToolGetPersonMergeContext, ids)
	require.NotEqual(true, contextResult["isError"])
	contextData := toolStructuredContent(t, contextResult)
	survivor, ok := contextData["survivor"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(left.ID), survivor["id"], 0)
	assert.NotEmpty(contextData["survivor_etag"])
	args := map[string]any{"survivor_person_id": float64(left.ID), "absorbed_person_id": float64(right.ID),
		"survivor_etag": contextData["survivor_etag"], "absorbed_etag": contextData["absorbed_etag"], "idempotency_key": "merge-mcp-1"}
	for _, code := range []string{"invalid_if_match", "invalid_idempotency_key"} {
		args["survivor_etag"] = contextData["survivor_etag"]
		args["idempotency_key"] = "merge-mcp-1"
		if code == "invalid_if_match" {
			args["survivor_etag"] = "malformed-etag"
		} else {
			args["idempotency_key"] = strings.Repeat("x", 129)
		}
		result := confirmedCallTool(t, opts, ToolMergePerson, args, true)
		assert.Equal(true, result["isError"])
		assert.Contains(fmt.Sprint(result["content"]), code)
	}
	args["idempotency_key"] = "merge-mcp-1"
	args["survivor_etag"] = fmt.Sprintf(`"person-%d-r999"`, left.ID)
	stale := confirmedCallTool(t, opts, ToolMergePerson, args, true)
	assert.Equal(true, stale["isError"])
	assert.Contains(fmt.Sprint(stale), "person_merge_revision_conflict")
	args["survivor_etag"] = contextData["survivor_etag"]
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_publications (person_id, desired) VALUES (?, TRUE)`), right.ID)
	require.NoError(err)
	blocked := confirmedCallTool(t, opts, ToolMergePerson, args, true)
	assert.Equal(true, blocked["isError"])
	assert.Contains(fmt.Sprint(blocked), "person_carddav_published")
	_, err = st.GetPersonContext(t.Context(), right.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM carddav_publications WHERE person_id = ?`), right.ID)
	require.NoError(err)
	args["idempotency_key"] = "merge-mcp-2"
	merged := confirmedCallTool(t, opts, ToolMergePerson, args, true, func(message string) {
		assert.Contains(message, `"Right"`)
		assert.Contains(message, `"Left"`)
		assert.Contains(message, "into")
	})
	require.NotEqual(true, merged["isError"], fmt.Sprint(merged))
	mergedPerson, ok := toolStructuredContent(t, merged)["person"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(left.ID), mergedPerson["id"], 0)
	// The absorbed profile is gone; a prompt read must not block a valid retry.
	retried := confirmedCallTool(t, opts, ToolMergePerson, args, true)
	require.NotEqual(true, retried["isError"], fmt.Sprint(retried))
	assert.Equal(toolStructuredContent(t, merged), toolStructuredContent(t, retried))
}

func TestMCPCardDAVUnavailableErrorIsActionable(t *testing.T) {
	backend := newIdentityReviewMCPBackend(t)
	result := confirmedCallTool(t, ServeOptions{Engine: &querytest.MockEngine{}, PersonCardDAV: backend, AllowCardDAVWrites: true},
		ToolSyncCardDAV, map[string]any{}, true)
	assert.Equal(t, true, result["isError"])
	assert.Contains(t, fmt.Sprint(result["content"]), "carddav_unavailable")
}

func TestMCPPersonCardDAVCompletionPublicationAndSyncRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	photoData := strings.Repeat("QUJD", 12*1024)
	privateCard := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Private\\, Test\r\nPHOTO:data:image/png;base64," + photoData + "\r\nEMAIL:contact@example.com\r\nEND:VCARD\r\n"
	var approved atomic.Bool
	var syncBlocked atomic.Bool
	syncBlocked.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/carddav/publications/7":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"person_id":7,"state":"unpublished","desired":false,"inference_review_required":true}`))
		case "/api/v1/carddav/publications/7/preview":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = fmt.Fprintf(w, `{"person_id":7,"address_book":{"id":2,"name":"Personal"},"kind":"pending","vcard":%q,"approval_token":"token-1","review_required":true}`, privateCard)
		case "/api/v1/carddav/publications/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			var body struct {
				Token string `json:"approval_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				assert.NoError(err)
				return
			}
			if body.Token != "token-1" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"carddav_review_stale","message":"Private Test must not appear here"}`))
				return
			}
			approved.Store(true)
			_, _ = w.Write([]byte(`{"person_id":7,"state":"pending","desired":true,"pending_operation":"create"}`))
		case "/api/v1/carddav/sync":
			assert.Equal(http.MethodPost, r.Method)
			if syncBlocked.Load() {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"carddav_inference_review_required","message":"Private Test must not appear here"}`))
				return
			}
			_, _ = w.Write([]byte(`{"books":1,"created":0,"updated":0,"removed":0}`))
		case "/api/v1/carddav/status":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"available":true,"configured":true,"credential_configured":true,"enabled":true,"scheduled":false,"schedule":"","account":{"username":"contact-owner@example.com","base_url":"https://contacts.example.com"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PersonCardDAV: client, AllowCardDAVWrites: true}
	publication := rawCallTool(t, opts, ToolGetCardDAVPublication, map[string]any{"person_id": float64(7)})
	assert.Equal(true, toolStructuredContent(t, publication)["inference_review_required"])
	preview := rawCallTool(t, opts, ToolPreviewCardDAVPublication, map[string]any{"person_id": float64(7)})
	assert.Equal(privateCard, toolStructuredContent(t, preview)["vcard"])
	assert.Equal("token-1", toolStructuredContent(t, preview)["approval_token"])
	stale := confirmedCallTool(t, opts, ToolApproveCardDAVPublication, map[string]any{"person_id": float64(7), "approval_token": "old-token"}, true)
	assert.Equal(true, stale["isError"])
	assert.Contains(fmt.Sprint(stale), "carddav_review_stale")
	assert.NotContains(fmt.Sprint(stale), "Private Test")
	assert.False(approved.Load())
	approvedResult := confirmedCallTool(t, opts, ToolApproveCardDAVPublication, map[string]any{"person_id": float64(7), "approval_token": "token-1"}, true, func(message string) {
		assert.Contains(message, `"Private, Test" (person 7)`)
		assert.Contains(message, "Personal")
		assert.Contains(message, "queues")
		assert.Contains(message, "[inline PHOTO, 49174 encoded bytes]")
		assert.Contains(message, "EMAIL:contact@example.com")
		assert.NotContains(message, photoData)
		assert.Less(len(message), 2048)
	})
	assert.NotEqual(true, approvedResult["isError"])
	assert.True(approved.Load())
	assert.Equal("pending", toolStructuredContent(t, approvedResult)["state"])
	blockedSync := confirmedCallTool(t, opts, ToolSyncCardDAV, map[string]any{}, true)
	assert.Equal(true, blockedSync["isError"])
	assert.Contains(fmt.Sprint(blockedSync), "carddav_inference_review_required")
	assert.NotContains(fmt.Sprint(blockedSync), "Private Test")
	syncBlocked.Store(false)
	syncResult := confirmedCallTool(t, opts, ToolSyncCardDAV, map[string]any{}, true, func(message string) {
		assert.Contains(message, "contact-owner@example.com")
		assert.Contains(message, "https://contacts.example.com")
		assert.Contains(message, "incremental")
	})
	counts := toolStructuredContent(t, syncResult)
	assert.InDelta(1.0, counts["books"], 0)
	for _, key := range []string{"created", "updated", "removed"} {
		assert.InDelta(0.0, counts[key], 0)
	}
	status := rawCallTool(t, opts, ToolGetCardDAVSyncStatus, map[string]any{})
	assert.Equal(true, toolStructuredContent(t, status)["available"])
}

// FuzzMCPCardDAVApprovalInlineMedia checks that payload bytes never reach the
// confirmation, across all four inline media properties. The HTTP fixture is
// the daemon boundary; MCP and daemonclient execute their production paths.
func FuzzMCPCardDAVApprovalInlineMedia(f *testing.F) {
	f.Add([]byte{0, 1, 2}, uint8(0))
	f.Add([]byte{}, uint8(1))
	f.Add([]byte{255}, uint8(2))
	f.Add([]byte("synthetic media"), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, variant uint8) {
		property := []string{"PHOTO", "LOGO", "SOUND", "KEY"}[variant%4]
		value := "data:;base64," + base64.StdEncoding.EncodeToString(data)
		body := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Example Contact\r\n" + property + ":" + value + "\r\nEMAIL:contact@example.com\r\nEND:VCARD\r\n"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/carddav/publications/7/preview":
				_, _ = fmt.Fprintf(w, `{"person_id":7,"address_book":{"id":2,"name":"Example"},"kind":"pending","vcard":%q,"approval_token":"token-1","review_required":true}`, body)
			case "/api/v1/carddav/publications/7/approve":
				_, _ = w.Write([]byte(`{"person_id":7,"state":"pending","desired":true,"inference_review_required":false}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(server.Close)
		client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, client.Close()) })
		opts := ServeOptions{Engine: &querytest.MockEngine{}, PersonCardDAV: client, AllowCardDAVWrites: true}
		result := confirmedCallTool(t, opts, ToolApproveCardDAVPublication,
			map[string]any{"person_id": float64(7), "approval_token": "token-1"}, true, func(message string) {
				assert.NotContains(t, message, "data:;base64,")
				assert.Contains(t, message, "[inline "+property+",")
			})
		require.NotEqual(t, true, result["isError"])
	})
}
