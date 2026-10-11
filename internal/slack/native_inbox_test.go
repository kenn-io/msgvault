package slack

import (
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

// The native HTTP fixtures deliberately reuse a root timestamp in two channels.
// A timestamp alone cannot authorize a Slack thread in the other channel.
func nativeSlackInboxWorkspace(t *testing.T) *fakeSlack {
	t.Helper()
	f := nativeEventsWorkspace(t)
	f.convs[0].Msgs[0].Replies = []fakeMsg{{TS: ts(1), ThreadTS: ts(0), User: "USENDER", Text: "synthetic historical reply"}}
	f.convs = append(f.convs,
		&fakeConv{ID: "COTHER", Name: "synthetic-other", Kind: "public", Members: []string{"UME", "USENDER"}, Msgs: []fakeMsg{{TS: ts(0), User: "USENDER", Text: "synthetic different root", Replies: []fakeMsg{{TS: ts(2), ThreadTS: ts(0), User: "USENDER", Text: "synthetic other reply"}}}}},
		&fakeConv{ID: "CQUIET", Name: "synthetic-quiet", Kind: "public", Members: []string{"UME", "USENDER"}},
	)
	return f
}

func enableNativeSlackInboxEvents(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "slack", Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: "slack", Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: "slack", Kinds: []string{"message"}},
	}})
	Require.NoError(t, err)
}

func TestNativeSlackInboxDiscoveryKeepsExactThreadParents(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := nativeSlackInboxWorkspace(t)
	imp, opts := testImporter(t, f)
	enableNativeSlackInboxEvents(t, imp.store)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	require.Len(page.Resources, 5, "three authenticated channels (including quiet) and two observed native threads")
	channels := map[string]store.MCPResource{}
	threads := map[string]store.MCPResource{}
	for _, resource := range page.Resources {
		switch resource.ScopeKind {
		case "slack_channel":
			channels[resource.ProviderID] = resource
		case "slack_thread":
			threads[resource.ParentProviderID] = resource
		default:
			assert.Fail("unexpected native Slack scope kind", resource.ScopeKind)
		}
	}
	require.Len(channels, 3)
	require.Len(threads, 2)
	require.Contains(channels, "CQUIET")
	require.Contains(threads, "CEVENTS")
	require.Contains(threads, "COTHER")
	assert.NotEqual(threads["CEVENTS"].Reference, threads["COTHER"].Reference, "identical root timestamps in different channels are distinct resources")
	f.convs[0].Name = "synthetic-renamed"
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	renamed, err := imp.store.GetMCPResource(t.Context(), "inbox", channels["CEVENTS"].ID)
	require.NoError(err)
	assert.Equal(channels["CEVENTS"].Reference, renamed.Reference, "a display rename preserves the exact native channel")
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(0, count, "discovery and renamed historical overlap emit no arrivals")
}

func TestNativeSlackInboxReplyCapturesChannelAndExactRoot(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := nativeSlackInboxWorkspace(t)
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackInboxEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	liveTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
	f.convs[0].Msgs[0].Replies = append(f.convs[0].Msgs[0].Replies, fakeMsg{TS: liveTS, ThreadTS: ts(0), User: "USENDER", Text: "synthetic live exact-thread reply"})
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(4, count, "one live reply publishes conversation, source, channel, and exact thread occurrences")
	rows, err := imp.store.DB().Query(`SELECT s.scope_kind,s.parent_provider_id FROM mcp_event_log e JOIN native_message_scopes s ON s.id=e.scope_id WHERE e.family='msgvault.inbox_message_archived'`)
	require.NoError(err)
	defer func() { Assert.NoError(t, rows.Close()) }()
	memberships := map[string]string{}
	for rows.Next() {
		var kind, parent string
		require.NoError(rows.Scan(&kind, &parent))
		memberships[kind] = parent
	}
	require.NoError(rows.Err())
	assert.Equal(map[string]string{"slack_channel": "", "slack_thread": "CEVENTS"}, memberships, "the other channel's root cannot capture this reply")
}

// Import options cannot turn a different authenticated workspace/user into
// native authority. The real auth.test response must bind the selected source.
func TestNativeSlackInboxRejectsWrongAuthenticatedAccount(t *testing.T) {
	for _, field := range []string{"workspace", "user"} {
		t.Run(field, func(t *testing.T) {
			native := nativeSlackInboxWorkspace(t)
			imp, opts := testImporter(t, native)
			enableNativeSlackInboxEvents(t, imp.store)
			if field == "workspace" {
				opts.TeamID = "TOTHER"
			} else {
				opts.UserID = "UOTHER"
			}
			_, err := imp.Import(t.Context(), opts)
			Require.Error(t, err, "native auth.test identity must match the exact configured account")
			var count int
			Require.NoError(t, imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			Assert.Equal(t, 0, count, "wrong-account native data must not be archived under the selected source")
		})
	}
}

func TestNativeSlackInboxThreadCannotOutliveChannel(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	native := nativeSlackInboxWorkspace(t)
	imp, opts := testImporter(t, native)
	enableNativeSlackInboxEvents(t, imp.store)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
	require.NoError(err)
	var threadID int64
	require.NoError(imp.store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE scope_kind='slack_thread' AND parent_provider_id='CEVENTS'`).Scan(&threadID))
	require.NoError(imp.store.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: sum.SourceID, SourceReference: source.Reference, SourceType: "slack", NativeIdentity: opts.TeamID + ":" + opts.UserID, Kind: "slack_channel", RetiredProviderIDs: []string{"CEVENTS"}}))
	_, err = imp.store.GetMCPResource(t.Context(), "inbox", threadID)
	require.Error(err, "positive channel retirement must invalidate its retained thread resources")
	// A delayed native thread observation must not resurrect a child after its
	// exact parent has been retired by another fenced native observation.
	err = imp.store.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: sum.SourceID, SourceReference: source.Reference, SourceType: "slack", NativeIdentity: opts.TeamID + ":" + opts.UserID, Kind: "slack_thread", ParentProviderID: "CEVENTS", Scopes: []store.NativeMessageScope{{ProviderID: "CEVENTS:" + ts(0), DisplayName: "Synthetic retained thread"}}})
	require.Error(err, "a current account alone cannot authorize a retired channel's thread")
	_, err = imp.store.GetMCPResource(t.Context(), "inbox", threadID)
	assert.Error(err, "rejected delayed observation cannot make the old child discoverable")
}
