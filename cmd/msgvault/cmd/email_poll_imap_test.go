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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
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

	bodies        atomic.Int32
	accepted      atomic.Int32
	limitNext     atomic.Bool
	limited       atomic.Int32
	limitCommand  string
	limitBodyOnly bool
	limitCount    int32
	limitDelay    string
	limitDelays   []string
	lastLimitAt   atomic.Int64
	nextCommand   atomic.Int64
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
			command := strings.ToUpper(strings.Join(fields[1:], " "))
			if c.owner.limitCount > 0 && c.owner.limited.Load() >= c.owner.limitCount &&
				pollingIMAPReadCommand(command) {
				c.owner.nextCommand.CompareAndSwap(0, time.Now().UnixNano())
			}
			bodyFetch := strings.Contains(line, "BODY.PEEK[]") || strings.Contains(line, "BODY[]")
			matches := c.owner.limitCommand != "" &&
				strings.HasPrefix(command, strings.ToUpper(c.owner.limitCommand)) &&
				(!c.owner.limitBodyOnly || bodyFetch)
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
			limitIndex := int(c.owner.limited.Load())
			if limitIndex < len(c.owner.limitDelays) {
				delay = c.owner.limitDelays[limitIndex]
			}
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
				return n, fmt.Errorf("write IMAP fixture response: %w", err)
			}
			if written == 0 {
				return n, io.ErrShortWrite
			}
			out = out[written:]
		}
	}
	return n, nil
}

func pollingIMAPReadCommand(command string) bool {
	// Count only read commands guarded by the production retry-floor gate.
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "LIST", "STATUS", "SELECT", "ENABLE":
		return true
	case "UID":
		return len(fields) > 1 && (fields[1] == "SEARCH" || fields[1] == "FETCH")
	default:
		return false
	}
}

type observeIMAPLimitHandler struct {
	cancel   context.CancelFunc
	observed *atomic.Int32
	signal   chan struct{}
}

func (h observeIMAPLimitHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h observeIMAPLimitHandler) Handle(_ context.Context, record slog.Record) error {
	if strings.HasPrefix(record.Message, "IMAP command limited;") {
		if h.observed != nil {
			h.observed.Add(1)
		}
		if h.signal != nil {
			select {
			case h.signal <- struct{}{}:
			default:
			}
		}
		if h.cancel != nil {
			h.cancel()
		}
	}
	return nil
}
func (h observeIMAPLimitHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h observeIMAPLimitHandler) WithGroup(string) slog.Handler     { return h }

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
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), src.ID, blob))
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
			require.ErrorContains(t, err, "filter")
			for _, table := range []string{"messages", "sync_runs", "account_identities", "imap_folder_state"} {
				var n int
				require.NoError(t, st.Store.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
				assert.Zero(t, n, "filter preflight precedes %s writes", table)
			}
		})
	}
}

func TestRapidEmailIMAPRetryFloorDefersFollowupAfterObservedLimitCancellation(t *testing.T) {
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
	var callbacks, observedLimits atomic.Int32
	var firstErr error
	firstDone := make(chan struct{})
	s := scheduler.New(func(ctx context.Context, identifier string) error {
		if callbacks.Add(1) > 1 {
			return nil
		}
		defer close(firstDone)
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		state.logger = slog.New(observeIMAPLimitHandler{cancel: cancel, observed: &observedLimits})
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
	assert.EqualValues(1, observedLimits.Load(), "the server-directed limit was observed before cancellation")
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

func TestRapidEmailIMAPPositiveLimitReleasesArchiveGateBeforeRetryFloor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	counted := &pollingIMAPListener{Listener: ln, limitCommand: "LIST", limitDelay: "60"}
	counted.limitNext.Store(true)
	testutil.ServeIMAPMemServer(t, counted, map[string]int{"INBOX": 0}, nil)
	cfg, ic := pollingIMAPConfig(t, ln.Addr().String(), true)
	cfg.Analytics.AutoBuildCache = false
	st := storetest.New(t)
	src := pollingIMAPSource(t, st.Store, ic)
	state := testInvocationWithConfig(cfg)
	var observedLimits atomic.Int32
	limitObserved := make(chan struct{}, 1)
	state.logger = slog.New(observeIMAPLimitHandler{observed: &observedLimits, signal: limitObserved})

	type syncResult struct {
		err         error
		contextLive bool
	}
	gate := api.NewSerialOperationGate()
	callbackStarted := make(chan struct{}, 1)
	startRapidSync := make(chan struct{})
	var startOnce sync.Once
	start := func() { startOnce.Do(func() { close(startRapidSync) }) }
	firstResult := make(chan syncResult, 1)
	archiveAccount := "archive@example.test"
	archiveStarted := make(chan time.Time, 1)
	var imapCallbacks, archiveCallbacks atomic.Int32
	s := scheduler.New(func(ctx context.Context, identifier string) error {
		if identifier == src.Identifier {
			imapCallbacks.Add(1)
			callbackStarted <- struct{}{}
			<-startRapidSync
			err := runRapidEmailSync(withInvocation(ctx, state), identifier, st.Store, nil, state)
			firstResult <- syncResult{err: err, contextLive: ctx.Err() == nil}
			return err
		}
		archiveCallbacks.Add(1)
		archiveStarted <- time.Now()
		return nil
	}).WithWorkTracker(labelWorkTracker(gate, "a scheduled sync")).WithAccountPreemptionPolicy(func(identifier string) bool {
		return scheduledSyncPreemptible(t.Context(), st.Store, identifier, state.logger)
	})
	defer func() {
		start()
		<-s.Stop().Done()
	}()
	require.NoError(s.AddAccount(src.Identifier, "0 0 1 1 *"))
	require.NoError(s.AddAccount(archiveAccount, "0 0 1 1 *"))
	require.NoError(s.SetAccountPollInterval(src.Identifier, 30*time.Second))
	require.NoError(s.TriggerSync(src.Identifier, false))
	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		require.FailNow("the scheduler did not start the rapid acquisition")
	}

	require.NoError(s.TriggerSync(archiveAccount, false))
	require.Eventually(func() bool {
		for _, status := range s.Status() {
			if status.Email == archiveAccount {
				return status.Queued && !status.Running && status.StartedAt.IsZero()
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the unrelated scheduled archive pass is visibly queued behind this gate holder")
	_, _, held := gate.Holder()
	require.True(held, "the IMAP pass continues to own the shared scheduled-work gate")
	select {
	case <-archiveStarted:
		require.FailNow("the second scheduled pass started before the first released the gate")
	default:
	}

	start()
	select {
	case <-limitObserved:
	case <-time.After(5 * time.Second):
		require.FailNow("the real IMAP LIMIT response was not observed")
	}
	assert.EqualValues(1, observedLimits.Load(), "the non-canceling logger observes the provider deferral")
	assert.EqualValues(1, counted.limited.Load(), "the synthetic server sent one positive LIMIT response")

	var result syncResult
	select {
	case result = <-firstResult:
	case <-time.After(5 * time.Second):
		require.FailNow("rapid IMAP acquisition held the shared archive gate through the provider retry delay")
	}
	assert.True(result.contextLive, "the rapid callback returns before cleanup cancels its context")
	var acquisition *scheduler.AcquisitionError
	require.ErrorAs(result.err, &acquisition)
	require.True(acquisition.NotBefore.After(time.Now()), "the response retains its live provider deadline")
	assert.EqualValues(1, counted.accepted.Load(), "the first acquisition uses one provider connection")

	var archiveAdmissionAt time.Time
	select {
	case archiveAdmissionAt = <-archiveStarted:
	case <-time.After(5 * time.Second):
		require.FailNow("the unrelated scheduled archive pass did not acquire the gate after rapid deferral")
	}
	assert.True(archiveAdmissionAt.Before(acquisition.NotBefore), "the second scheduled pass acquires the shared gate before the provider deadline")
	assert.EqualValues(1, imapCallbacks.Load(), "the queued pass does not start another provider acquisition")
	assert.EqualValues(1, archiveCallbacks.Load(), "the unrelated scheduled pass enters after gate release")
	assert.EqualValues(1, counted.accepted.Load(), "the queued pass opens no second provider connection")
}

func TestRapidEmailIMAPPartialLimitDefersManualFollowup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	counted := &pollingIMAPListener{
		Listener: ln, limitCommand: "UID FETCH", limitBodyOnly: true,
		limitCount: 4, limitDelays: []string{"0", "0", "0", "60"},
	}
	user := testutil.ServeIMAPMemServer(t, counted, map[string]int{"INBOX": 0}, nil)
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", "rapid-floor@example.test")
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
	}, 5*time.Second, 10*time.Millisecond, "manual follow-up remains admitted but deferred under the provider floor")
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
	counted := &pollingIMAPListener{
		Listener: ln, limitCommand: "UID FETCH", limitBodyOnly: true,
		limitCount: 4, limitDelays: []string{"0", "0", "0", "60"},
	}
	user := testutil.ServeIMAPMemServer(t, counted, map[string]int{"Archive": 0, "INBOX": 0}, nil)
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", "rapid-floor-archive@example.test")
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", "rapid-floor-inbox@example.test")
	cfg, ic := pollingIMAPConfig(t, ln.Addr().String(), true)
	st := storetest.New(t)
	src := pollingIMAPSource(t, st.Store, ic)
	state := testInvocationWithConfig(cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = runRapidEmailSync(withInvocation(ctx, state), src.Identifier, st.Store, nil, state)
	var acquisition *scheduler.AcquisitionError
	require.ErrorAs(err, &acquisition)
	require.EqualValues(4, counted.limited.Load(), "one mailbox FETCH exhausted the real command retry budget")
	lastLimitNanos := counted.lastLimitAt.Load()
	nextCommandNanos := counted.nextCommand.Load()
	require.NotZero(lastLimitNanos)
	if nextCommandNanos == 0 {
		require.True(acquisition.NotBefore.After(time.Now()), "a blocked command may return without sending but must preserve the live floor")
		return
	}
	assert.Equal(scheduler.AcquisitionIncomplete, acquisition.Outcome)
	lastLimit := time.Unix(0, lastLimitNanos)
	nextCommand := time.Unix(0, nextCommandNanos)
	assert.GreaterOrEqual(nextCommand.Sub(lastLimit), 60*time.Second, "no later read command is sent while the provider floor is live")
}
