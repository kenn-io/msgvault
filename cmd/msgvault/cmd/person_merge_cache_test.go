package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonMergeRelatedDriftKeepsDerivedRefresh(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := newMeetingCountSeriesFixture(t, 1)
	secondID, err := f.st.EnsureParticipant("second@example.org", "Second", "example.org")
	requirements.NoError(err)
	first, _, err := f.st.CreatePersonFromParticipant(f.person)
	requirements.NoError(err)
	second, _, err := f.st.CreatePersonFromParticipant(secondID)
	requirements.NoError(err)
	_, err = buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[0], store.OccurrenceRef{MessageID: f.event, Key: "20260314T100000Z"}))
	previous := executeBuildCacheSubprocessMode
	var modes []buildCacheMode
	executeBuildCacheSubprocessMode = func(_ context.Context, mode buildCacheMode) error {
		modes = append(modes, mode)
		requirements.Equal(buildCacheModeDerived, mode)
		_, err := buildCacheDerivedOnly(f.path, f.cache)
		return err
	}
	t.Cleanup(func() { executeBuildCacheSubprocessMode = previous })
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: f.st, analyticsDir: f.cache}, Logger: slog.New(slog.DiscardHandler)})
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/people/%d/merge", first.ID), strings.NewReader(fmt.Sprintf(`{"absorbed_person_id":%d}`, second.ID)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", fmt.Sprintf(`"person-%d-r%d", "person-%d-r%d"`, first.ID, first.Revision, second.ID, second.Revision))
	request.Header.Set("Idempotency-Key", "merge-related-drift")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal([]buildCacheMode{buildCacheModeDerived}, modes)
	var pending int
	requirements.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&pending))
	assertions.Positive(pending, "the next scheduled build must consume the meeting repair")
}
