package cmd

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	xapi "go.kenn.io/msgvault/internal/x"
)

func TestXCheckpointLateParentAndRemovalViaDaemonReads(t *testing.T) {
	f := newXCollectionFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	f.adapter.xCleanup = cleaner
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: f.cfg, Store: f.adapter, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()),
		Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead,
	})
	cleaner.SetServer(server)
	host := httptest.NewServer(server.Router())
	t.Cleanup(host.Close)
	_, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--json")
	require.NoError(t, err)
	providerCalls := f.calls.Load()
	get := func(path string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", f.cfg.Server.APIKey)
		response, err := host.Client().Do(req)
		require.NoError(t, err)
		defer func() { assert.NoError(t, response.Body.Close()) }()
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, string(data))
		return data
	}
	write := func(post xapi.Post, author xapi.User, fetched time.Time) {
		t.Helper()
		require.NoError(t, xapi.WritePage(t.Context(), f.st, f.source.ID, "1001", xapi.Page{
			Posts: []xapi.Post{post}, Users: []xapi.User{author}, FetchedAt: fetched,
		}, fetched))
	}
	write(xapi.Post{
		ID: "802", AuthorID: "1002", ConversationID: "801", Text: "synthetic delayed child",
		Raw:       []byte(`{"id":"802","text":"synthetic delayed child"}`),
		CreatedAt: f.at, EditHistory: []string{"802"}, References: []xapi.Reference{{Type: "replied_to", ID: "801"}},
	}, xapi.User{ID: "1002", Username: "example_author", Name: "Example Author", ProtectedKnown: true}, f.at)
	child, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "802")
	require.NoError(t, err)
	var parent sql.NullInt64
	readParent := func() {
		t.Helper()
		require.NoError(t, f.st.DB().QueryRow(f.st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), child.MessageID).Scan(&parent))
	}
	readParent()
	assert.False(t, parent.Valid)
	threadPath := fmt.Sprintf("/api/v1/cli/message/thread?id=%d", child.MessageID)
	threadIDs := func() []int64 {
		t.Helper()
		var page query.ThreadPage
		require.NoError(t, json.Unmarshal(get(threadPath), &page))
		ids := make([]int64, len(page.Messages))
		for i, message := range page.Messages {
			ids[i] = message.ID
		}
		assert.Equal(t, int64(len(ids)), page.Total)
		return ids
	}
	assert.Equal(t, []int64{child.MessageID}, threadIDs())
	childDetailPath := fmt.Sprintf("/api/v1/messages/%d", child.MessageID)
	var childDetail api.MessageDetail
	require.NoError(t, json.Unmarshal(get(childDetailPath), &childDetail))
	assert.Equal(t, "synthetic delayed child", childDetail.Body)
	assert.False(t, childDetail.IsFromMe)

	// The original native parent arrives months later as a newer edit. The
	// production writer must repair the old child through its retained alias.
	write(xapi.Post{
		ID: "803", AuthorID: "1001", ConversationID: "801", Text: "synthetic late parent",
		Raw:       []byte(`{"id":"803","text":"synthetic late parent"}`),
		CreatedAt: f.at.Add(-time.Hour), EditHistory: []string{"801", "803"},
	}, xapi.User{ID: "1001", Username: "example_owner", Name: "Example Owner", ProtectedKnown: true}, f.at.AddDate(0, 4, 0))
	root, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "801")
	require.NoError(t, err)
	readParent()
	require.True(t, parent.Valid)
	assert.Equal(t, root.MessageID, parent.Int64)
	childAfter, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "802")
	require.NoError(t, err)
	assert.Equal(t, child.MessageID, childAfter.MessageID)
	assert.ElementsMatch(t, []int64{root.MessageID, child.MessageID}, threadIDs())
	detailPath := fmt.Sprintf("/api/v1/messages/%d", root.MessageID)
	var detail api.MessageDetail
	require.NoError(t, json.Unmarshal(get(detailPath), &detail))
	assert.True(t, detail.IsFromMe)
	assert.Equal(t, "synthetic late parent", detail.Body)

	_, _, err = f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--remove-post", "801", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(get(detailPath), &detail))
	assert.Empty(t, detail.Body)
	require.NoError(t, json.Unmarshal(get(childDetailPath), &childDetail))
	assert.Equal(t, "synthetic delayed child", childDetail.Body)
	assert.ElementsMatch(t, []int64{root.MessageID, child.MessageID}, threadIDs())
	readParent()
	require.True(t, parent.Valid)
	assert.Equal(t, root.MessageID, parent.Int64)
	assert.Equal(t, providerCalls, f.calls.Load(), "late-parent repair, daemon reads and local removal make no provider requests")
}
