package cmd

import (
	"encoding/json/v2"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Count full BODY[] requests at the real protocol boundary. Header metadata
// used to verify an existing identity is deliberately not a body download.
type pollingIMAPListener struct {
	net.Listener
	bodies atomic.Int32
}
type pollingIMAPConn struct {
	net.Conn
	owner   *pollingIMAPListener
	pending string
}

func (l *pollingIMAPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &pollingIMAPConn{Conn: conn, owner: l}, nil
}
func (c *pollingIMAPConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.pending += string(p[:n])
	for {
		line, rest, found := strings.Cut(c.pending, "\r\n")
		if !found {
			break
		}
		c.pending = rest
		line = strings.ToUpper(line)
		if strings.Contains(line, "BODY.PEEK[]") || strings.Contains(line, "BODY[]") {
			c.owner.bodies.Add(1)
		}
	}
	return n, err
}

func pollingIMAPConfig(t *testing.T, address string, rapid bool) (*config.Config, *imaplib.Config) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	ic := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	interval := "0s"
	if rapid {
		interval = "30s"
	}
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`[[accounts]]
email = %q
schedule = "0 * * * *"
enabled = true
poll_interval = %q
`, ic.Identifier(), interval)), 0o600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	require.NoError(t, imaplib.SaveCredentials(cfg.TokensDir(), ic.Identifier(), testutil.IMAPTestPassword))
	return cfg, ic
}

func pollingIMAPSource(t *testing.T, st *store.Store, ic *imaplib.Config) *store.Source {
	t.Helper()
	src, err := st.GetOrCreateSource("imap", ic.Identifier())
	require.NoError(t, err)
	blob, err := ic.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(src.ID, blob))
	src, err = st.GetSourceByID(src.ID)
	require.NoError(t, err)
	return src
}

func TestRapidEmailIMAPRefreshesFlagsWithoutBodyDownload(t *testing.T) {
	for _, rapid := range []bool{false, true} {
		t.Run(fmt.Sprintf("rapid=%t", rapid), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			counted := &pollingIMAPListener{Listener: ln}
			user := testutil.ServeIMAPMemServer(t, counted, map[string]int{"INBOX": 1, "Projects": 1}, nil)
			cfg, ic := pollingIMAPConfig(t, ln.Addr().String(), rapid)
			st := storetest.New(t)
			src := pollingIMAPSource(t, st.Store, ic)
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			run := func() {
				t.Helper()
				summary, err := runScheduledIMAPSync(ctx, src, st.Store, invocationFromContext(ctx))
				require.NoError(t, err)
				require.NotNil(t, summary)
				require.Zero(t, summary.Errors)
			}
			run()
			bodies := counted.bodies.Load()
			require.Positive(t, bodies)
			client, err := imapclient.DialInsecure(ln.Addr().String(), nil)
			require.NoError(t, err)
			require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
			_, err = client.Select("INBOX", nil).Wait()
			require.NoError(t, err)
			var uids imap.UIDSet
			uids.AddNum(1)
			require.NoError(t, client.Store(uids, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}}, nil).Close())
			require.NoError(t, client.Close())
			run()
			assert.Equal(t, bodies, counted.bodies.Load(), "flag refresh of known exact IDs uses metadata only")
			var rawFlags string
			require.NoError(t, st.Store.DB().QueryRow(st.Store.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox='INBOX' AND uid=1`), src.ID).Scan(&rawFlags))
			var flags []string
			require.NoError(t, json.Unmarshal([]byte(rawFlags), &flags))
			if rapid {
				assert.Contains(t, flags, `\Seen`)
				assert.Contains(t, flags, `\Flagged`)
			} else {
				assert.NotContains(t, flags, `\Seen`, "ordinary accounts retain the UIDNEXT/count shortcut")
				assert.NotContains(t, flags, `\Flagged`)
			}
			// Each new pass rediscovers topology and mail delivered outside INBOX.
			require.NoError(t, user.Create("Later", nil))
			testutil.AppendIMAPMessage(t, user, "Later")
			run()
			var messages int
			require.NoError(t, st.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(t, 3, messages)
		})
	}
}

func TestRapidEmailIMAPRejectsEffectiveFilterBeforeMutation(t *testing.T) {
	for _, filter := range []string{"source", "global"} {
		t.Run(filter, func(t *testing.T) {
			addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
			cfg, ic := pollingIMAPConfig(t, addr, true)
			if filter == "source" {
				ic.Folders = []string{"INBOX"}
			} else {
				cfg.Sync.IMAPSkipFolders = map[string][]string{ic.Identifier(): {"Other"}}
			}
			st := storetest.New(t)
			src := pollingIMAPSource(t, st.Store, ic)
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			err := runScheduledSync(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx))
			assert.ErrorContains(t, err, "filter")
			for _, table := range []string{"messages", "sync_runs", "account_identities", "imap_folder_state"} {
				var n int
				require.NoError(t, st.Store.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
				assert.Zero(t, n, "filter preflight precedes %s writes", table)
			}
		})
	}
}
