package sync

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncRemoteImagesRequireOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("gmail", "user@example.com")
			require.NoError(err)
			var requests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
			}))
			defer upstream.Close()
			var fetcher *remoteimage.Fetcher
			if enabled {
				fetcher = remoteimage.NewFetcher()
				fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
				}
				fetcher.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
				}
			}
			raw := []byte("From: user@example.com\r\nTo: other@example.com\r\nSubject: Receipt\r\nContent-Type: text/html\r\n\r\n<img src=\"http://images.example/chart\">")
			syncer := New(nil, st, &Options{AttachmentsDir: t.TempDir(), RemoteImages: fetcher})
			_, err = syncer.ingestMessage(t.Context(), src.ID, &gmail.RawMessage{ID: "message", Raw: raw}, "thread", nil)
			require.NoError(err)
			var id int64
			require.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&id))
			refs, err := st.MessageRemoteImages(id)
			require.NoError(err)
			if enabled {
				assert.Len(refs, 1)
				assert.Equal(int64(1), requests.Load())
			} else {
				assert.Empty(refs)
				assert.Zero(requests.Load())
			}
			saved, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(raw, saved)
		})
	}
}
