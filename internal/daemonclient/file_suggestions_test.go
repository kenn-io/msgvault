package daemonclient_test

import (
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSuggestPersonFilesTransport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	pid, err := f.Store.EnsureParticipant("member@example.test", "Member Example", "example.test")
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(pid)
	require.NoError(err)
	inside := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	for i, tc := range []struct {
		name, mime string
		when       time.Time
		role       string
	}{
		{"report.pdf", "application/pdf", inside, "to"},
		{"reprot.pdf", "application/pdf", inside, "cc"},
		{"report.txt", "text/plain", inside, "to"},
		{"report.pdf", "application/pdf", inside.AddDate(0, 0, 30), "to"},
		{"report.pdf", "application/pdf", inside, "from"},
	} {
		mid := f.CreateMessage(fmt.Sprintf("transport-%d", i))
		require.NoError(f.Store.ReplaceMessageRecipients(mid, tc.role, []int64{pid}, []string{"Member Example"}))
		_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), tc.when, mid)
		require.NoError(err)
		_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,storage_path) VALUES(?,?,?,'')`), mid, tc.name, tc.mime)
		require.NoError(err)
	}
	ready := false
	for range 100 {
		b, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(err)
		if b.Status.Phase == "ready" && !b.More {
			ready = true
			break
		}
	}
	require.True(ready)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: f.Store, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(client.Close()) })
	after, before := inside.AddDate(0, 0, -1), inside.AddDate(0, 0, 1)
	request := daemonclient.PersonFileSuggestionOptions{PersonID: person.ID, Filename: "report.pdf", Directions: []personscope.Direction{personscope.ToPerson}, After: &after, Before: &before, MIMEFamilies: []query.FileMIMEFamily{query.FileMIMEPDF}, Limit: 1}
	response, err := client.SuggestPersonFiles(t.Context(), request)
	require.NoError(err)
	require.Len(response.Suggestions, 1)
	assert.Equal("report.pdf", response.Suggestions[0].Filename)
	assert.Equal([]int64{pid}, response.Suggestions[0].PersonProvenance.ParticipantIds)
	assert.False(response.CandidateLimitReached)
	assert.NotEmpty(response.IndexRevision)
	request.Limit = 0
	response, err = client.SuggestPersonFiles(t.Context(), request)
	require.NoError(err)
	require.Len(response.Suggestions, 2)
	assert.Equal("reprot.pdf", response.Suggestions[1].Filename)
	for _, invalid := range []int{-1, 101} {
		request.Limit = invalid
		_, err = client.SuggestPersonFiles(t.Context(), request)
		require.Error(err, "invalid limit must not be silently omitted")
	}
	request.Limit = 0
	_, err = f.Store.DB().Exec(`DELETE FROM filename_discovery_state`)
	require.NoError(err)
	_, err = client.SuggestPersonFiles(t.Context(), request)
	require.Error(err)
	var apiErr *daemonclient.APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(503, apiErr.Status)
	assert.Equal("filename_index_unavailable", apiErr.Code)
}
