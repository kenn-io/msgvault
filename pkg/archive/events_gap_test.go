//go:build cgo

package archive_test

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

// A program embedding msgvault does not capture MCP Events. Opening an
// archive whose capture was on must record the gap, so a daemon restarted
// later cannot resume subscriptions as if those library writes never happened.
func TestOpenSQLiteRecordsEventsCaptureGap(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "msgvault.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	require.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "library-gap@example.test")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "library-gap-thread", "Library gap")
	require.NoError(err)
	clock, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)
	now := time.Now().UTC()
	subscription := store.MCPSubscription{
		ID: "sub_library_gap", Principal: "owner:synthetic", Name: "msgvault.message_archived",
		Arguments: []byte(`{"conversation_id":"` + strconv.FormatInt(conversation, 10) + `","include_from_me":false}`),
		ScopeKind: "conversation", ScopeID: conversation, CallbackURL: "https://receiver.example.net/events",
		SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(st.BindMCPSubscriptionScope(t.Context(), &subscription))
	_, _, err = st.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: subscription, Now: now})
	require.NoError(err)
	require.NoError(st.Close())

	opened, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(opened.Close()) })

	var epoch int64
	var enabled bool
	require.NoError(opened.Store().DB().QueryRow(`SELECT capture_epoch, enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch, &enabled))
	assert.Greater(epoch, clock.Epoch, "the library open starts a new capture epoch")
	assert.False(enabled)
	stopped, err := opened.Store().GetMCPSubscription(t.Context(), subscription.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("capture_gap", stopped.StopReason, "an unknown owner records a gap, not a revocation")
}

// Opening an archive that never enabled Events records nothing.
func TestOpenSQLiteLeavesEventsOffArchiveUnchanged(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "msgvault.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	opened, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, opened.Close()) })
	var epoch int64
	require.NoError(opened.Store().DB().QueryRow(`SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch))
	assert.Zero(t, epoch)
}

// A daemon that keeps running after a library open turned capture off must
// not activate subscriptions in the new epoch: the library's writes are not
// captured, so those subscriptions would silently miss them.
func TestRunningDaemonRefusesActivationAfterLibraryGap(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "msgvault.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	daemon, err := store.Open(path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(daemon.Close()) })
	source, err := daemon.GetOrCreateSource("gmail", "running-daemon@example.test")
	require.NoError(err)
	conversation, err := daemon.EnsureConversation(source.ID, "running-daemon-thread", "Running daemon")
	require.NoError(err)
	_, err = daemon.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)
	now := time.Now().UTC()
	subscription := store.MCPSubscription{
		ID: "sub_running_daemon", Principal: "owner:synthetic", Name: "msgvault.message_archived",
		Arguments: []byte(`{"conversation_id":"` + strconv.FormatInt(conversation, 10) + `","include_from_me":false}`),
		ScopeKind: "conversation", ScopeID: conversation, CallbackURL: "https://receiver.example.net/events",
		SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(daemon.BindMCPSubscriptionScope(t.Context(), &subscription))

	library, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(library.Close()) })

	_, _, err = daemon.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: subscription, Now: now})
	require.Error(err)
	assert.Equal("events_unavailable", err.Error())
}
