package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDaemonMessageTagsMicrosoftSharedPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeMSMail, "owner@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread-1", "Tags")
	require.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "immutable-1", MessageType: "email"})
	require.NoError(err)
	folder, err := st.EnsureLabel(source.ID, "folder-1", "Inbox", "system")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(mid, []int64{folder}, true)
	require.NoError(err)
	cfg := lifecycleTestConfig(t.TempDir())
	tokenPath := microsoft.NewGraphMailManager("", "", "", cfg.TokensDir(), nil).TokenPath(source.Identifier)
	require.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	saveScopes := func(scopes []string) {
		data, encodeErr := json.Marshal(map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "scopes": scopes})
		require.NoError(encodeErr)
		require.NoError(os.WriteFile(tokenPath, data, 0o600))
	}
	writes := new(atomic.Int32)
	tags := []string{"Other"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/me/messages/immutable-1", r.URL.Path)
		if r.Method == http.MethodPatch {
			writes.Add(1)
			var body struct {
				Categories []string `json:"categories"`
			}
			if decodeErr := json.UnmarshalRead(r.Body, &body); decodeErr != nil {
				assert.Fail("decode Graph categories", "%v", decodeErr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tags = body.Categories
		}
		assert.NoError(json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": tags, "@odata.etag": `W/"v1"`}))
	}))
	t.Cleanup(srv.Close)
	a := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), microsoftTagClientFactory: func(context.Context, *store.Source, bool) (*msmail.Client, error) {
		return msmail.NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000), nil
	}}
	saveScopes(microsoft.GraphMailScopes())
	_, err = a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}, DryRun: true}, "", nil)
	require.NoError(err, "previews need only read scopes")
	assert.Zero(writes.Load())
	_, err = a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}}, "", nil)
	var failure *emailtags.MessageTagError
	require.ErrorAs(err, &failure)
	assert.Equal("insufficient_scope", failure.Code)
	assert.Zero(writes.Load())
	saveScopes(microsoft.GraphMailWriteScopes())
	got, err := a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}}, "", nil)
	require.NoError(err)
	assert.True(got.Verified)
	assert.Equal(mid, got.MessageID)
	msg, err := st.GetMessage(mid)
	require.NoError(err)
	assert.ElementsMatch([]string{"Inbox", "Category: Other", "Category: Next"}, msg.Labels)
	assert.Equal(int32(1), writes.Load())
}
