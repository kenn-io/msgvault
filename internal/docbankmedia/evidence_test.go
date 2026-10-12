package docbankmedia_test

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/docbankmedia"
)

// Mirrors of Docbank v0.15.1's metadata request; strict decoding rejects any
// member the server would reject.
type wireSelect struct {
	NodeID    int64  `json:"node_id"`
	Revision  int64  `json:"revision"`
	VersionID string `json:"version_id"`
	BlobHash  string `json:"blob_hash"`
	Size      int64  `json:"size"`
	Profile   string `json:"profile"`
}

func TestEvidenceWindowWire(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const version = "11111111-1111-4111-8111-111111111111"
	content, attachment, build, rendition := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	var mu sync.Mutex
	var selected wireSelect
	unprocessed := false
	receiptState := "ready"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(testKey, r.Header.Get("X-Api-Key")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes/7":
			writeJSON(w, map[string]any{"id": 7, "revision": 1, "name": "voice.wav", "kind": "file", "current_version_id": version})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes/8":
			writeJSON(w, map[string]any{"id": 8, "current_version_id": version, "trashed_at": "2026-10-05T00:00:00Z"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/versions/"+version:
			writeJSON(w, map[string]any{"id": version, "node_id": 7, "blob_hash": content, "size": 44})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/renditions/text":
			if !assert.NoError(json.UnmarshalRead(r.Body, &selected, json.RejectUnknownMembers(true))) || unprocessed {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, map[string]any{"state": receiptState, "source": map[string]any{"node_id": selected.NodeID, "revision": selected.Revision,
				"version_id": selected.VersionID, "blob_hash": selected.BlobHash, "size": selected.Size}, "profile": map[string]any{"name": selected.Profile},
				"attachment_id": attachment, "build_id": build, "artifact": map[string]any{"sha256": rendition}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/evidence/windows":
			var request docbankmedia.EvidenceWindowRequest
			if !assert.NoError(json.UnmarshalRead(r.Body, &request, json.RejectUnknownMembers(true))) {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			for _, value := range []string{request.VaultUID, request.ContentVersionID} {
				id, err := uuid.Parse(value)
				if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != value {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":400,"code":"invalid_evidence_request"}`))
					return
				}
			}
			text := []rune("Send the revised budget by Friday.")
			if request.Offset > len(text) {
				http.Error(w, "offset past end", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			text = text[request.Offset:min(len(text), request.Offset+request.MaxChars)]
			writeJSON(w, docbankmedia.EvidenceWindow{VaultUID: request.VaultUID, NodeID: request.NodeID, ContentVersionID: request.ContentVersionID,
				ContentSHA256: request.ContentSHA256, RenditionAttachmentID: request.RenditionAttachmentID, BuildID: request.BuildID, RenditionSHA256: request.RenditionSHA256,
				Text: string(text), ActualStart: request.Offset, ActualEnd: request.Offset + len(text), NextOffset: request.Offset + len(text), MediaType: "text/markdown"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := docbankmedia.NewClient(server.URL, func() (string, error) { return testKey, nil })
	require.NoError(err)

	identity, err := client.EvidenceIdentity(t.Context(), version, content, "supplied-transcript")
	require.NoError(err)
	assert.Equal(docbankmedia.EvidenceWindowRequest{NodeID: 7, ContentVersionID: version, ContentSHA256: content, RenditionAttachmentID: attachment, BuildID: build, RenditionSHA256: rendition}, identity)
	mu.Lock()
	assert.Equal(int64(7), selected.NodeID)
	assert.Equal("supplied-transcript", selected.Profile)
	assert.Equal(int64(1), selected.Revision)
	assert.Equal(int64(44), selected.Size)
	mu.Unlock()

	identity.VaultUID, identity.Offset, identity.MaxChars = "22222222-2222-4222-8222-222222222222", 9, 7
	window, err := client.ReadEvidenceWindow(t.Context(), identity)
	require.NoError(err)
	assert.Equal("revised", window.Text)

	for state, want := range map[string]error{
		"failed":                 docbankmedia.ErrRenditionFailed,
		"historical_unavailable": docbankmedia.ErrEvidenceUnavailable,
		"verified_empty":         docbankmedia.ErrEvidenceUnavailable,
		"unconfigured":           docbankmedia.ErrProfileUnavailable,
		"profile_required":       docbankmedia.ErrProfileUnavailable,
		"unprocessed":            docbankmedia.ErrNoRendition,
		"pending":                docbankmedia.ErrInvalidReceipt,
		"unknown":                docbankmedia.ErrInvalidReceipt,
		"":                       docbankmedia.ErrInvalidReceipt,
	} {
		mu.Lock()
		receiptState = state
		mu.Unlock()
		identity, err := client.EvidenceIdentity(t.Context(), version, content, "supplied-transcript")
		require.ErrorIs(err, want, "receipt state %q", state)
		_, isHTTP := errors.AsType[*docbankmedia.HTTPError](err)
		assert.False(isHTTP, "receipt state %q is not a Docbank HTTP failure", state)
		if state == "unprocessed" {
			assert.Equal(int64(7), identity.NodeID, "the caller asks the node why nothing was selected")
		}
	}
	mu.Lock()
	receiptState = "ready"
	mu.Unlock()

	_, err = client.EvidenceIdentity(t.Context(), version, strings.Repeat("e", 64), "supplied-transcript")
	require.ErrorIs(err, docbankmedia.ErrEvidenceUnavailable, "another file's bytes")

	mu.Lock()
	unprocessed = true
	mu.Unlock()
	unselected, err := client.EvidenceIdentity(t.Context(), version, content, "supplied-transcript")
	httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err)
	require.True(ok, "%v", err)
	assert.Equal(http.StatusNotFound, httpErr.Status)
	assert.Equal(int64(7), unselected.NodeID, "the caller asks the node why nothing was selected")

	current, err := client.CurrentVersion(t.Context(), 7)
	require.NoError(err)
	assert.Equal(version, current)
	for _, node := range []int64{8, 9} {
		_, err = client.CurrentVersion(t.Context(), node)
		require.ErrorIs(err, docbankmedia.ErrEvidenceUnavailable, "a trashed or deleted node")
	}
	_, err = client.EvidenceIdentity(t.Context(), "33333333-3333-4333-8333-333333333333", content, "supplied-transcript")
	require.ErrorIs(err, docbankmedia.ErrEvidenceUnavailable, "a pruned version")
}
