package importer

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMIMERemoteImagesCommitWithReadySnapshot(t *testing.T) {
	for _, failAttachment := range []bool{false, true} {
		t.Run(fmt.Sprintf("attachment-failure=%t", failAttachment), func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-image-snapshot", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			var requests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var count int
				err := st.DB().QueryRowContext(r.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count)
				Assert.NoError(t, err)
				Assert.Zero(t, count, "selected remote image preparation precedes the canonical message commit")
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
			}))
			defer upstream.Close()
			fetcher := remoteimage.NewFetcher()
			fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			fetcher.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
			}
			if failAttachment {
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_remote_snapshot_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic remote snapshot failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_remote_snapshot_failure BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION synthetic_remote_snapshot_failure()`)
				} else {
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_remote_snapshot_failure BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic remote snapshot failure'); END`)
				}
				require.NoError(err)
			}
			tmp := t.TempDir()
			mailbox := filepath.Join(tmp, "Synthetic.mailbox")
			require.NoError(os.MkdirAll(mailbox, 0o700))
			raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <synthetic-remote-ready@example.net>\r\nSubject: Synthetic image snapshot\r\nContent-Type: text/html\r\n\r\n<img src=\"http://images.example/chart\">")
			require.NoError(os.WriteFile(filepath.Join(mailbox, "message.eml"), raw, 0o600))
			summary, err := ImportEMLDir(t.Context(), st, mailbox, EMLImportOptions{Identifier: "owner@example.net", NotifyImports: true, NoResume: true, RemoteImages: fetcher, AttachmentsDir: filepath.Join(tmp, "attachments")})
			require.NoError(err)
			require.NotNil(summary)
			assert.Equal(int64(1), requests.Load())
			if failAttachment {
				assert.True(summary.HardErrors)
				for _, table := range []string{"messages", "participants", "conversations", "message_bodies", "message_raw", "attachments", "import_message_decisions", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
					assert.Zero(count, "selected remote image write rolls back %s", table)
				}
				_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_remote_snapshot_failure`+func() string {
					if st.IsPostgreSQL() {
						return " ON attachments"
					}
					return ""
				}())
				require.NoError(err)
				summary, err = ImportEMLDir(t.Context(), st, mailbox, EMLImportOptions{Identifier: "owner@example.net", NotifyImports: true, NoResume: true, RemoteImages: fetcher, AttachmentsDir: filepath.Join(tmp, "attachments")})
				require.NoError(err)
				require.NotNil(summary)
				assert.Equal(int64(2), requests.Load(), "failed canonical transaction remains retryable")
			}
			assert.False(summary.HardErrors)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_id FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&messageID))
			refs, err := st.MessageRemoteImages(messageID)
			require.NoError(err)
			assert.Len(refs, 1)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT attachment_count FROM messages WHERE id=?`), messageID).Scan(&count))
			assert.Equal(1, count)
		})
	}
}
