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
func categoryClient(t *testing.T, status int) (*Client, *atomic.Int32) {
	t.Helper()
	assert := assert.New(t)
	writes := new(atomic.Int32)
	tags := []string{"Old", "Other"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/me/messages/immutable-1", r.URL.Path)
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
		assert.NoError(json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": tags, "@odata.etag": `W/"version-1"`}))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000), writes
}

func TestMessageTagsMicrosoftCategories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, writes := categoryClient(t, http.StatusOK)
	change := &emailtags.MessageTagChange{Add: []string{"Next"}, Remove: []string{"Old"}, DryRun: true}
	preview, err := c.MessageTags(t.Context(), "immutable-1", change)
	require.NoError(err)
	assert.ElementsMatch([]string{"Next", "Other"}, preview.Tags)
	assert.True(preview.DryRun)
	assert.Zero(writes.Load())
	change.DryRun = false
	got, err := c.MessageTags(t.Context(), "immutable-1", change)
	require.NoError(err)
	assert.True(got.Verified)
	assert.ElementsMatch([]string{"Old", "Other"}, got.Before)
	assert.ElementsMatch([]string{"Next", "Other"}, got.Tags)
	assert.Equal(int32(1), writes.Load())
	_, err = c.MessageTags(t.Context(), "immutable-1", change)
	require.NoError(err)
	assert.Equal(int32(1), writes.Load())
}

func TestMessageTagsMicrosoftCategoryRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change emailtags.MessageTagChange
		want   []string
	}{
		{"case preserved", emailtags.MessageTagChange{Add: []string{"old", "Next", "NEXT"}, Remove: []string{"OTHER"}}, []string{"Old", "Next"}},
		{"clear categories", emailtags.MessageTagChange{Remove: []string{"Old", "Other"}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, writes := categoryClient(t, http.StatusOK)
			change, err := emailtags.Normalize(tc.change, "msmail")
			require.NoError(err)
			got, err := c.MessageTags(t.Context(), "immutable-1", &change)

			require.NoError(err)
			assert.ElementsMatch(tc.want, got.Tags)
			assert.True(got.Verified)
			assert.Equal(int32(1), writes.Load())
		})
	}
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
		{"throttled", http.StatusTooManyRequests, "provider_write_failed"},
		{"uncertain write", http.StatusInternalServerError, "remote_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, writes := categoryClient(t, tc.status)
			got, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.MessageTagChange{Add: []string{"Next"}})
			var failure *emailtags.MessageTagError
			require.ErrorAs(err, &failure)
			assert.Equal(tc.code, failure.Code)
			require.NotNil(got)
			assert.False(got.Verified)
			assert.ElementsMatch([]string{"Old", "Other"}, got.Tags)
			assert.Equal(int32(1), writes.Load())
		})
	}
}

func TestMessageTagsMicrosoftReadbackFailureKeepsObservedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"matching", `{"id":"immutable-1","categories":["Other","Next"]}`, []string{"Other", "Next"}},
		{"different message", `{"id":"other-message","categories":["Next"]}`, []string{"Other"}},
		{"malformed", `{`, []string{"Other"}},
		{"missing tags", `{"id":"immutable-1"}`, []string{"Other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			reads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					_, _ = w.Write([]byte(tc.body))
					return
				}
				reads++
				if reads > 1 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`{"id":"immutable-1","categories":["Other"],"@odata.etag":"version-1"}`))
			}))
			t.Cleanup(srv.Close)
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			result, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.MessageTagChange{Add: []string{"Next"}})
			var failure *emailtags.MessageTagError
			require.ErrorAs(err, &failure)
			assert.Equal("verification_failed", failure.Code)
			require.NotNil(result)
			assert.Equal([]string{"Other"}, result.Before)
			assert.Equal(tc.want, result.Tags)
			assert.False(result.Verified)
			assert.Equal(result, failure.Result)
			assert.Equal(2, reads)
		})
	}
}

func TestMessageTagsMicrosoftIncompleteSnapshotDoesNotWrite(t *testing.T) {
	for _, scenario := range []string{"wrong identity", "missing version", "missing categories"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			writes := new(atomic.Int32)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				body := map[string]any{"id": "immutable-1", "categories": []string{}, "@odata.etag": `W/"v1"`}
				switch scenario {
				case "wrong identity":
					body["id"] = "different-message"
				case "missing version":
					delete(body, "@odata.etag")
				default:
					delete(body, "categories")
				}
				assert.NoError(json.MarshalWrite(w, body))
			}))
			t.Cleanup(srv.Close)
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			_, err := c.MessageTags(t.Context(), "immutable-1", &emailtags.MessageTagChange{Add: []string{"Next"}})
			require.Error(err)
			assert.Zero(writes.Load())
		})
	}
}
