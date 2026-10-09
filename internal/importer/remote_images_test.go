package importer

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEmlxRemoteImageFailureIsWarning(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, tmp := openTestStore(t)
	requirements.True(st.FTS5Available())
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	fetcher := remoteimage.NewFetcher()
	fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	fetcher.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}
	raw := []byte("From: sender@example.test\r\nTo: owner@example.test\r\n" +
		"Message-ID: <optional-image@example.test>\r\nSubject: Optional image\r\n" +
		"Content-Type: text/html\r\n\r\n<p>optionalimageneedle</p><img src=\"http://images.example/chart\">")
	root := filepath.Join(tmp, "Inbox.mbox")
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	var logs bytes.Buffer
	opts := EmlxImportOptions{
		Identifier: "owner@example.test", RemoteImages: fetcher,
		AttachmentsDir: filepath.Join(tmp, "attachments"),
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
	}
	for _, phase := range []string{"first", "repeat", "full reconciliation"} {
		t.Run(phase, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			logs.Reset()
			opts.FullReconcile = phase == "full reconciliation"
			summary, err := ImportEmlxDir(t.Context(), st, root, opts)
			requirements.NoError(err)
			assertions.False(summary.HardErrors, "optional image fetching must remain best-effort")
			assertions.Zero(summary.Errors)
			assertions.Equal(int64(1), summary.MessagesProcessed)
			if phase != "repeat" {
				assertions.Contains(logs.String(), "failed to archive remote image")
			}
			var messageID int64
			requirements.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&messageID))
			saved, err := st.GetMessageRawContext(t.Context(), messageID)
			requirements.NoError(err)
			assertions.Equal(raw, saved)
			labels, err := st.MessageLabelIDsContext(t.Context(), messageID)
			requirements.NoError(err)
			assertions.Len(labels, 1)
			var indexed, receipts int
			requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'optionalimageneedle'").Scan(&indexed))
			assertions.Equal(1, indexed)
			requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM source_import_items WHERE provider = 'emlx-occurrence' AND COALESCE(checksum, '') <> ''").Scan(&receipts))
			assertions.Zero(receipts, "remote image dependencies cannot authorize reusable filesystem receipts")
			refs, err := st.MessageRemoteImages(messageID)
			requirements.NoError(err)
			assertions.Empty(refs)
		})
	}
	assertions.Equal(int64(2), requests.Load(), "first import and forced reconciliation exercise the real failed fetch")
}

func TestRawImportRemoteImagesRequireOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("eml", "user@example.com")
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
			err = rawMessageIngester(fetcher)(t.Context(), st, src.ID, "user@example.com", t.TempDir(), nil, "message", "hash", raw, time.Time{}, slog.Default())
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
