package archive_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &decoded), body)
	return decoded
}

func exportedMessages(t *testing.T, body string) int {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var complete struct {
		RecordType string `json:"record_type"`
		Counts     struct {
			Messages int `json:"messages"`
		} `json:"counts"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &complete), body)
	require.Equal(t, "complete", complete.RecordType, body)
	return complete.Counts.Messages
}

// The CLI read commands' operations stay inside the host's source scope, so a
// host can serve list-accounts, stats, search, show-message, show-thread, and
// export-messages to a caller limited to some sources.
func TestLibrarySourceScopeCLIReads(t *testing.T) {
	st, _, handler, sources := newSourceScopeFixture(t)
	allowed, other := sources[0], sources[1]
	unscoped := t.Context()
	scoped := archive.WithSourceScope(t.Context(), []int64{allowed.source.ID})
	nothing := archive.WithSourceScope(t.Context(), nil)
	const window = "start=2025-12-01T00:00:00Z&end=2026-02-01T00:00:00Z"
	get := func(ctx context.Context, path string) (int, string) {
		w := sourceScopeRequest(ctx, t, handler, path)
		return w.Code, w.Body.String()
	}

	t.Run("health", func(t *testing.T) {
		code, body := get(scoped, "/api/v1/health")
		assert.Equal(t, http.StatusOK, code, body)
	})

	t.Run("stats", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ctx  context.Context
			want int
		}{{"unscoped", unscoped, 4}, {"scoped", scoped, 2}, {"empty scope", nothing, 0}} {
			code, body := get(tc.ctx, "/api/v1/cli/stats")
			require.Equal(t, http.StatusOK, code, body)
			var stats struct {
				Stats struct {
					TotalMessages int `json:"total_messages"`
				} `json:"stats"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &stats), body)
			assert.Equal(t, tc.want, stats.Stats.TotalMessages, "%s: %s", tc.name, body)
		}
		code, body := get(scoped, "/api/v1/cli/stats?account="+other.source.Identifier)
		assert.NotEqual(t, http.StatusOK, code, "an account outside the scope is refused: %s", body)
	})

	t.Run("search", func(t *testing.T) {
		code, body := get(unscoped, "/api/v1/cli/search?q=telescope")
		require.Equal(t, http.StatusOK, code, body)
		assert.Contains(t, body, "source-1", "the baseline sees both sources")
		code, body = get(scoped, "/api/v1/cli/search?q=telescope")
		require.Equal(t, http.StatusOK, code, body)
		assert.Contains(t, body, "source-0")
		assert.NotContains(t, body, "source-1")
		assert.Len(t, decodeJSON(t, body)["results"], 2)
		code, body = get(nothing, "/api/v1/cli/search?q=telescope")
		require.Equal(t, http.StatusOK, code, body)
		assert.Empty(t, decodeJSON(t, body)["results"])
	})

	t.Run("message", func(t *testing.T) {
		code, body := get(scoped, fmt.Sprintf("/api/v1/cli/message?id=%d", allowed.messages[0]))
		assert.Equal(t, http.StatusOK, code, body)
		code, body = get(scoped, fmt.Sprintf("/api/v1/cli/message?id=%d", other.messages[0]))
		assert.Equal(t, http.StatusNotFound, code, body)
		assert.NotContains(t, body, "telescope")
	})

	t.Run("thread", func(t *testing.T) {
		code, body := get(scoped, fmt.Sprintf("/api/v1/cli/message/thread?id=%d", allowed.messages[0]))
		require.Equal(t, http.StatusOK, code, body)
		assert.Len(t, decodeJSON(t, body)["messages"], 2)
		code, body = get(scoped, fmt.Sprintf("/api/v1/cli/message/thread?id=%d", other.messages[0]))
		assert.Equal(t, http.StatusNotFound, code, body)
		assert.NotContains(t, body, "telescope")
	})

	t.Run("export", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ctx  context.Context
			want int
		}{{"unscoped", unscoped, 4}, {"scoped", scoped, 2}, {"empty scope", nothing, 0}} {
			code, body := get(tc.ctx, "/api/v1/cli/messages/export?"+window)
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, tc.want, exportedMessages(t, body), tc.name)
		}
		code, body := get(scoped, "/api/v1/cli/messages/export?"+window+"&source=gmail:"+allowed.source.Identifier)
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, 2, exportedMessages(t, body))
		code, body = get(scoped, "/api/v1/cli/messages/export?"+window+"&source=gmail:"+other.source.Identifier)
		assert.Equal(t, http.StatusNotFound, code, "a source outside the scope reads as missing: %s", body)
	})

	t.Run("export person", func(t *testing.T) {
		participant, err := st.EnsureParticipant("outsider@example.com", "Outsider", "example.com")
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(t, err)
		contactOnly, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
			Name: "Contact Only", Emails: []store.PersonCreateContact{{Value: "contact@example.org", Type: "work"}},
		})
		require.NoError(t, err)
		standalone, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{Name: "Name Only"})
		require.NoError(t, err)
		path := func(id int64) string {
			return fmt.Sprintf("/api/v1/cli/messages/export?%s&person_id=%d", window, id)
		}
		existing, missing := path(person.ID), path(person.ID+1000)
		code, body := get(unscoped, missing)
		assert.NotEqual(t, http.StatusOK, code, "an unscoped caller learns the person is missing: %s", body)
		for _, ctx := range []context.Context{scoped, nothing} {
			for _, path := range []string{existing, missing, path(contactOnly.ID), path(standalone.ID)} {
				code, body := get(ctx, path)
				require.Equal(t, http.StatusOK, code, "a scoped caller cannot tell a missing person from an inaccessible one: %s", body)
				assert.Equal(t, 0, exportedMessages(t, body))
			}
		}
	})
}
