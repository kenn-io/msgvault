package msmail

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

// The server exercises the native Graph GET/PATCH contract, including the
// conditional category replacement that preserves concurrent provider edits.
func categoryClient(t *testing.T, status int, omit bool) (*Client, *atomic.Int32) {
	t.Helper()
	assert := assert.New(t)
	writes := new(atomic.Int32)
	tags := []string{"Old", "Other"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/me/messages/immutable-1", r.URL.Path)
		assert.Equal("Bearer synthetic-token", r.Header.Get("Authorization"))
		assert.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		if r.Method == http.MethodPatch {
			writes.Add(1)
			assert.Equal(`W/"version-1"`, r.Header.Get("If-Match"))
			var body map[string][]string
			if err := json.UnmarshalRead(r.Body, &body); err != nil {
				assert.Fail("decode Graph categories", "%v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Len(body, 1)
			assert.NotNil(body["categories"], "clearing all categories must send [], not null")
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			tags = body["categories"]
		}
		if omit {
			assert.NoError(json.MarshalWrite(w, map[string]any{"id": "immutable-1"}))
			return
		}
		assert.NoError(json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": tags, "@odata.etag": `W/"version-1"`}))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000), writes
}

func TestMessageTagsMicrosoftCategories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, writes := categoryClient(t, http.StatusOK, false)
	change := &emailtags.Change{Add: []string{"Next"}, Remove: []string{"Old"}, DryRun: true}
	preview, err := c.MessageTags(t.Context(), "immutable-1", change)
	require.NoError(err)
	assert.ElementsMatch([]string{"Next", "Other"}, preview.Tags)
	assert.True(preview.DryRun)
	assert.Zero(writes.Load())
	change.DryRun = false
	got, err := c.MessageTags(t.Context(), "immutable-1", change)
	require.NoError(err)
	assert.Equal(SourceType, got.Provider)
	assert.True(got.Verified)
	assert.ElementsMatch([]string{"Old", "Other"}, got.Before)
	assert.ElementsMatch([]string{"Next", "Other"}, got.Tags)
	assert.Equal(int32(1), writes.Load())
	_, err = c.MessageTags(t.Context(), "immutable-1", change)
	require.NoError(err)
	assert.Equal(int32(1), writes.Load())
}

func TestMessageTagsMicrosoftFailureDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"concurrent change", http.StatusPreconditionFailed, "stale_identity"},
		{"permission", http.StatusForbidden, "insufficient_scope"},
		{"invalid category", http.StatusBadRequest, "invalid_tag"},
		{"uncertain write", http.StatusInternalServerError, "remote_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, writes := categoryClient(t, tc.status, false)
			got, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.Change{Add: []string{"Next"}})
			var failure *emailtags.Error
			require.ErrorAs(err, &failure)
			assert.Equal(tc.code, failure.Code)
			require.NotNil(got)
			assert.False(got.Verified)
			assert.ElementsMatch([]string{"Old", "Other"}, got.Tags)
			assert.Equal(int32(1), writes.Load())
		})
	}
}

func TestMessageTagsMicrosoftMissingSnapshotDoesNotWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, writes := categoryClient(t, http.StatusOK, true)
	_, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.Change{Add: []string{"Next"}})
	require.Error(err)
	assert.Zero(writes.Load())
}

func TestMessageTagsMicrosoftClearCategories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, writes := categoryClient(t, http.StatusOK, false)
	got, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.Change{Remove: []string{"Old", "Other"}})
	require.NoError(err)
	assert.Empty(got.Tags)
	assert.True(got.Verified)
	assert.Equal(int32(1), writes.Load())
}

func TestMessageTagsMicrosoftReadbackFailureKeepsObservedSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	writes := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			writes.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		if writes.Load() > 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.NoError(json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": []string{"Other"}, "@odata.etag": `W/"v1"`}))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	got, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.Change{Add: []string{"Next"}})
	var failure *emailtags.Error
	require.ErrorAs(err, &failure)
	assert.Equal("verification_failed", failure.Code)
	require.NotNil(got)
	assert.False(got.Verified)
	assert.Equal([]string{"Other"}, got.Tags)
	assert.Equal(int32(1), writes.Load())
}

func TestMessageTagsMicrosoftRejectsWrongIdentityAndMissingVersion(t *testing.T) {
	for _, scenario := range []string{"wrong identity", "missing version"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			writes := new(atomic.Int32)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				body := map[string]any{"id": "immutable-1", "categories": []string{}, "@odata.etag": `W/"v1"`}
				if scenario == "wrong identity" {
					body["id"] = "different-message"
				} else {
					delete(body, "@odata.etag")
				}
				assert.NoError(json.MarshalWrite(w, body))
			}))
			t.Cleanup(srv.Close)
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			_, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.Change{Add: []string{"Next"}})
			require.Error(err)
			assert.Zero(writes.Load())
		})
	}
}
