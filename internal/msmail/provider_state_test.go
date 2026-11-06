package msmail

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// A real Graph response adds provider state to the existing transport fixture;
// omission on later responses must preserve the last authoritative snapshot.
func TestProviderStateReadFollowupAndMove(t *testing.T) {
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	state := map[string]any{"isRead": false, "flag": map[string]any{"flagStatus": "flagged"}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.serve(rec, r)
		body := rec.Body.Bytes()
		if strings.Contains(r.URL.Path, "/messages") && !strings.HasSuffix(r.URL.Path, "/$value") && rec.Code == http.StatusOK {
			if r.URL.Query().Get("token") == "" && r.URL.Query().Get("skip") == "" {
				fields := strings.Split(r.URL.Query().Get("$select"), ",")
				assert.Contains(t, fields, "isRead", "native Graph acquisition must request read state")
				assert.Contains(t, fields, "flag", "native Graph acquisition must request follow-up state")
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal(body, &payload))
			apply := func(item map[string]any) {
				for k, v := range state {
					item[k] = v
				}
			}
			if values, ok := payload["value"].([]any); ok {
				for _, value := range values {
					apply(value.(map[string]any))
				}
			} else {
				apply(payload)
			}
			var err error
			body, err = json.Marshal(payload)
			require.NoError(t, err)
		}
		for k, values := range rec.Header() {
			w.Header()[k] = values
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	f.srv = proxy // keep opaque delta links on the same fixture transport
	c := NewClient(proxy.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
	run := func() {
		t.Helper()
		_, err := Import(context.Background(), st, c, Options{Email: "sample@example.test", AttachmentsDir: t.TempDir()}, slog.Default())
		require.NoError(t, err)
	}
	run()
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "sample@example.test")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(t, err)
	labels := func() []string {
		t.Helper()
		rows, err := st.DB().Query(st.Rebind(`SELECT l.source_label_id FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=?`), ids["m1"])
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var result []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			result = append(result, id)
		}
		require.NoError(t, rows.Err())
		return result
	}
	assert.ElementsMatch(t, []string{"inbox", "msmail-state:unread", "msmail-state:flagged"}, labels())
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=TRUE WHERE id=?`), ids["m1"])
	require.NoError(t, err)
	state = nil
	f.put("m1", "archive")
	run()
	assert.ElementsMatch(t, []string{"archive", "msmail-state:unread", "msmail-state:flagged"}, labels())
	var localRead bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), ids["m1"]).Scan(&localRead))
	assert.True(t, localRead, "provider unread state must not overwrite local reader state")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), ids["m1"])
	require.NoError(t, err)
	state = map[string]any{"isRead": true, "flag": map[string]any{"flagStatus": "completed"}}
	f.put("m1", "archive")
	run()
	assert.Equal(t, []string{"archive"}, labels())
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), ids["m1"]).Scan(&localRead))
	assert.False(t, localRead, "provider read state must not overwrite local reader state")
}
