package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Count full BODY[] requests at the real protocol boundary. Header metadata
// used to verify an existing identity is deliberately not a body download.
type pollingIMAPListener struct {
	net.Listener

	bodies       atomic.Int32
	accepted     atomic.Int32
	limitNext    atomic.Bool
	limited      atomic.Int32
	limitCommand string
	limitCount   int32
	limitDelay   string
	lastLimitAt  atomic.Int64
	nextCommand  atomic.Int64
}
type pollingIMAPConn struct {
	net.Conn

	owner        *pollingIMAPListener
	pending      string
	writePending string
	limitTag     string
}

func (l *pollingIMAPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept IMAP fixture connection: %w", err)
	}
	l.accepted.Add(1)
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
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if c.owner.limitCount > 0 && c.owner.limited.Load() >= c.owner.limitCount {
				c.owner.nextCommand.CompareAndSwap(0, time.Now().UnixNano())
			}
			command := strings.ToUpper(strings.Join(fields[1:], " "))
			matches := c.owner.limitCommand != "" && strings.HasPrefix(command, strings.ToUpper(c.owner.limitCommand))
			if matches && (c.owner.limitNext.CompareAndSwap(true, false) || c.owner.limitCount > c.owner.limited.Load()) {
				c.limitTag = fields[0]
			}
		}
	}
	if err != nil {
		err = fmt.Errorf("read IMAP fixture connection: %w", err)
	}
	return n, err
}

func (c *pollingIMAPConn) Write(p []byte) (int, error) {
	n := len(p)
	c.writePending += string(p)
	for {
		line, rest, found := strings.Cut(c.writePending, "\r\n")
		if !found {
			break
		}
		c.writePending = rest
		fields := strings.Fields(line)
		if c.limitTag != "" && len(fields) >= 2 && fields[0] == c.limitTag && strings.EqualFold(fields[1], "OK") {
			delay := c.owner.limitDelay
			if delay == "" {
				delay = "60"
			}
			line = c.limitTag + " NO [LIMIT] retry in " + delay + " seconds"
			c.limitTag = ""
			c.owner.limited.Add(1)
			c.owner.lastLimitAt.Store(time.Now().UnixNano())
		}
		out := []byte(line + "\r\n")
		for len(out) > 0 {
			written, err := c.Conn.Write(out)
			if err != nil {
				return n, err
			}
			if written == 0 {
				return n, io.ErrShortWrite
			}
			out = out[written:]
		}
	}
	return n, nil
}

type cancelIMAPRetryHandler struct {
	cancel   context.CancelFunc
	observed *atomic.Int32
}

func (h cancelIMAPRetryHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h cancelIMAPRetryHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "IMAP command limited; waiting to retry" {
		h.observed.Add(1)
		h.cancel()
	}
	return nil
}
func (h cancelIMAPRetryHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h cancelIMAPRetryHandler) WithGroup(string) slog.Handler     { return h }

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
			assert := assert.New(t)
			require := require.New(t)
			run()
			bodies := counted.bodies.Load()
			require.Positive(bodies)
			client, err := imapclient.DialInsecure(ln.Addr().String(), nil)
			require.NoError(err)
			require.NoError(client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
			_, err = client.Select("INBOX", nil).Wait()
			require.NoError(err)
			var uids imap.UIDSet
			uids.AddNum(1)
			require.NoError(client.Store(uids, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}}, nil).Close())
			require.NoError(client.Close())
			run()
			assert.Equal(bodies, counted.bodies.Load(), "flag refresh of known exact IDs uses metadata only")
			var rawFlags string
			require.NoError(st.Store.DB().QueryRow(st.Store.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox='INBOX' AND uid=1`), src.ID).Scan(&rawFlags))
			var flags []string
			require.NoError(json.Unmarshal([]byte(rawFlags), &flags))
			if rapid {
				assert.Contains(flags, `\Seen`)
				assert.Contains(flags, `\Flagged`)
			} else {
				assert.NotContains(flags, `\Seen`, "ordinary accounts retain the UIDNEXT/count shortcut")
				assert.NotContains(flags, `\Flagged`)
			}
			// Each new pass rediscovers topology and mail delivered outside INBOX.
			require.NoError(user.Create("Later", nil))
			testutil.AppendIMAPMessage(t, user, "Later")
			run()
			var messages int
			require.NoError(st.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(3, messages)
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

func TestRapidEmailIMAPRetryFloorDefersFollowupAfterCanceledWait(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	counted := &pollingIMAPListener{Listener: ln, limitCommand: "LIST"}
	counted.limitNext.Store(true)
	testutil.ServeIMAPMemServer(t, counted, map[string]int{"INBOX": 0}, nil)
	cfg, ic := pollingIMAPConfig(t, ln.Addr().String(), true)
	st := storetest.New(t)
	src := pollingIMAPSource(t, st.Store, ic)
	state := testInvocationWithConfig(cfg)
	var callbacks, retryWaits atomic.Int32
	var firstErr error
	firstDone := make(chan struct{})
	s := scheduler.New(func(ctx context.Context, identifier string) error {
		if callbacks.Add(1) > 1 {
			return nil
		}
		defer close(firstDone)
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		state.logger = slog.New(cancelIMAPRetryHandler{cancel: cancel, observed: &retryWaits})
		firstErr = runRapidEmailSync(withInvocation(ctx, state), identifier, st.Store, nil, state)
		return firstErr
	})
	defer func() { <-s.Stop().Done() }()
	require.NoError(s.AddAccount(src.Identifier, "0 0 1 1 *"))
	require.NoError(s.SetAccountPollInterval(src.Identifier, 30*time.Second))
	require.NoError(s.TriggerSync(src.Identifier, false))
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		require.FailNow("the first rapid IMAP acquisition did not finish")
	}
	require.Eventually(func() bool {
		status := s.Status()[0]
		return !status.Running && !status.Queued
	}, 5*time.Second, 10*time.Millisecond, "the scheduler records the completed acquisition")
	var acquisition *scheduler.AcquisitionError
	require.ErrorAs(firstErr, &acquisition)
	assert.Equal(scheduler.AcquisitionCancelled, acquisition.Outcome)
	assert.EqualValues(1, retryWaits.Load(), "the server-directed retry wait was interrupted")
	assert.EqualValues(1, counted.limited.Load(), "the real IMAP response carried the provider limit")
	require.True(acquisition.NotBefore.After(time.Now()), "the cancelled pass retains the server retry deadline")
	assert.False(s.Status()[0].NextEligiblePoll.Before(acquisition.NotBefore), "the account lane preserves the provider floor")

	require.NoError(s.TriggerSync(src.Identifier, false))
	require.Eventually(func() bool {
		status := s.Status()[0]
		return status.Pending && !status.Running && !status.Queued
	}, 5*time.Second, 10*time.Millisecond, "the accepted follow-up remains deferred until the provider floor")
	assert.EqualValues(1, callbacks.Load(), "another manual acquisition waits for the provider floor")
	assert.EqualValues(1, counted.accepted.Load(), "the deferred acquisition opens no second IMAP connection")
	assert.True(s.Status()[0].Pending, "the accepted manual retry remains pending")
}

func TestRapidEmailIMAPPartialLimitDefersManualFollowup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	counted := &pollingIMAPListener{Listener: ln, limitCommand: "UID FETCH", limitCount: 4, limitDelay: "1"}
	testutil.ServeIMAPMemServer(t, counted, map[string]int{"INBOX": 1}, nil)
	cfg, ic := pollingIMAPConfig(t, ln.Addr().String(), true)
	st := storetest.New(t)
	src := pollingIMAPSource(t, st.Store, ic)
	state := testInvocationWithConfig(cfg)
	var callbacks atomic.Int32
	firstResult := make(chan error, 1)
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	s := scheduler.New(func(ctx context.Context, identifier string) error {
		if callbacks.Add(1) > 1 {
			close(secondStarted)
			<-releaseSecond
			return nil
		}
		err := runRapidEmailSync(withInvocation(ctx, state), identifier, st.Store, nil, state)
		firstResult <- err
		return err
	})
	defer func() {
		close(releaseSecond)
		<-s.Stop().Done()
	}()
	require.NoError(s.AddAccount(src.Identifier, "0 0 1 1 *"))
	require.NoError(s.SetAccountPollInterval(src.Identifier, 30*time.Second))
	require.NoError(s.TriggerSync(src.Identifier, false))
	var firstErr error
	select {
	case firstErr = <-firstResult:
	case <-time.After(10 * time.Second):
		require.FailNow("the partial IMAP acquisition did not return")
	}
	require.Eventually(func() bool {
		status := s.Status()[0]
		return !status.Running && !status.Queued
	}, 5*time.Second, 10*time.Millisecond, "the scheduler records the partial acquisition")
	var acquisition *scheduler.AcquisitionError
	require.ErrorAs(firstErr, &acquisition)
	assert.Equal(scheduler.AcquisitionIncomplete, acquisition.Outcome)
	require.EqualValues(4, counted.limited.Load(), "one FETCH exhausted the real command retry budget")
	require.True(acquisition.NotBefore.After(time.Now()), "the per-item partial result retains its provider deadline")
	assert.Positive(counted.bodies.Load(), "the limited request was the message body fetch")

	require.NoError(s.TriggerSync(src.Identifier, false))
	require.Eventually(func() bool {
		status := s.Status()[0]
		return status.Pending && !status.Running && !status.Queued
	}, 300*time.Millisecond, 5*time.Millisecond, "manual follow-up remains admitted but deferred under the provider floor")
	assert.EqualValues(1, callbacks.Load(), "the scheduler must not start another acquisition before the provider deadline")
	assert.True(s.Status()[0].Pending)
	assert.EqualValues(1, counted.accepted.Load(), "the deferred request opens no second IMAP connection")
	select {
	case <-secondStarted:
		require.FailNow("the scheduler started a follow-up while the provider floor was live")
	default:
	}
}

func TestRapidEmailIMAPPartialLimitGatesNextCommand(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	counted := &pollingIMAPListener{Listener: ln, limitCommand: "UID FETCH", limitCount: 4, limitDelay: "1"}
	testutil.ServeIMAPMemServer(t, counted, map[string]int{"Archive": 1, "INBOX": 1}, nil)
	cfg, ic := pollingIMAPConfig(t, ln.Addr().String(), true)
	st := storetest.New(t)
	src := pollingIMAPSource(t, st.Store, ic)
	state := testInvocationWithConfig(cfg)
	err = runRapidEmailSync(withInvocation(t.Context(), state), src.Identifier, st.Store, nil, state)
	var acquisition *scheduler.AcquisitionError
	require.ErrorAs(err, &acquisition)
	assert.Equal(scheduler.AcquisitionIncomplete, acquisition.Outcome)
	require.EqualValues(4, counted.limited.Load(), "one mailbox FETCH exhausted the real command retry budget")
	lastLimitNanos := counted.lastLimitAt.Load()
	nextCommandNanos := counted.nextCommand.Load()
	require.NotZero(lastLimitNanos)
	require.NotZero(nextCommandNanos, "the second mailbox must issue a command after the partial FETCH")
	lastLimit := time.Unix(0, lastLimitNanos)
	nextCommand := time.Unix(0, nextCommandNanos)
	assert.GreaterOrEqual(nextCommand.Sub(lastLimit), time.Second, "no later IMAP command is sent while the provider floor is live")
}
