package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestVirtualAccountSelectionScopesMessagesAndStats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := Model{mode: modeEmail, accounts: []query.AccountInfo{{ID: 7, Identifier: "inbox@example.net", VirtualAccounts: []store.VirtualAccount{
		{Key: "identity:7:d29ya0BleGFtcGxlLm9yZw", SourceID: 7, AccountAddress: "work@example.org", MessageCount: 3},
		{Key: "fastmail-masked:inbox@example.net", SourceID: 7, Group: "fastmail-masked:inbox@example.net", MessageCount: 4},
		{Key: "unattributed:7", SourceID: 7, Unattributed: true, MessageCount: 1},
	}}}}
	options := m.scopeOptions()
	require.Len(options, 5)
	s := virtualSourceScope(*options[2].virtualAccount)
	var f query.MessageFilter
	s.apply(&f)
	require.NotNil(f.SourceID)
	assert.Equal(int64(7), *f.SourceID)
	require.Len(f.AccountScopes, 1)
	assert.Equal([]string{"work@example.org"}, f.AccountScopes[0].Addresses)
	assert.True(s.matches(options[2]))
	assert.False(s.matches(options[1]))
	assert.Contains(s.title(m.accounts), "work@example.org")
}

func TestSingleIdentitySourceShowsPendingRepair(t *testing.T) {
	for _, pending := range []int64{0, 3} {
		t.Run(fmt.Sprintf("pending_%d", pending), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			m := Model{mode: modeEmail, accounts: []query.AccountInfo{{
				ID: 7, Identifier: "inbox@example.net", VirtualAccounts: []store.VirtualAccount{
					{Key: "identity:7:aW5ib3hAZXhhbXBsZS5uZXQ", SourceID: 7, AccountAddress: "inbox@example.net"},
					{Key: "unattributed:7", SourceID: 7, Unattributed: true, MessageCount: pending, PendingCount: pending},
				},
			}}}
			options := m.scopeOptions()
			if pending == 0 {
				require.Len(options, 2, "completed single-identity sources stay collapsed")
				return
			}
			require.Len(options, 4)
			require.NotNil(options[3].virtualAccount)
			assert.True(options[3].virtualAccount.Unattributed)
			assert.Contains(options[3].label, "3")
			assert.Contains(options[3].label, "repair")
			var filter query.MessageFilter
			virtualSourceScope(*options[3].virtualAccount).apply(&filter)
			require.Len(filter.AccountScopes, 1)
			assert.True(filter.AccountScopes[0].Unattributed)
		})
	}
}

func TestStageAllMatchesManifestPreservesVirtualAccountScope(t *testing.T) {
	setup := require.New(t)
	f := storetest.New(t)
	setup.NoError(f.Store.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	setup.NoError(f.Store.AddAccountIdentity(f.Source.ID, "mask@example.org", "fastmail-masked-email"))
	for _, tc := range []struct{ key, header string }{
		{"work", "work@example.org"}, {"mask", "mask@example.org"}, {"ambiguous", "work@example.org, mask@example.org"},
	} {
		id := f.CreateMessage(tc.key)
		setup.NoError(f.Store.UpsertMessageRaw(id, []byte("X-Delivered-To: "+tc.header+"\r\n\r\nbody")))
	}
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if f.Store.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(f.Store.DB(), dialect)
	for _, tc := range []struct {
		name  string
		scope search.AccountScope
		want  string
	}{
		{"identity", search.AccountScope{SourceID: &f.Source.ID, Addresses: []string{"work@example.org"}, Inbound: true}, "work"},
		{"group", search.AccountScope{SourceID: &f.Source.ID, Groups: []string{"fastmail-masked:" + f.Source.Identifier}}, "mask"},
		{"unattributed", search.AccountScope{SourceID: &f.Source.ID, Unattributed: true}, "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			scopes := []search.AccountScope{tc.scope}
			controller := NewActionController(e, t.TempDir(), nil)
			manifest, err := controller.StageForDeletion(DeletionContext{
				AllMatches: true, MatchFilter: query.MessageFilter{AccountScopes: scopes},
			})
			require.NoError(err)
			assert.Equal([]string{tc.want}, manifest.GmailIDs)
			var recorded struct {
				MatchFilter struct {
					AccountScopes []search.AccountScope `json:"account_scopes"`
				} `json:"match_filter"`
			}
			require.NoError(json.Unmarshal(manifest.RawFilter, &recorded))
			require.Equal(scopes, recorded.MatchFilter.AccountScopes)
			parsed := search.Parse(strings.Join(search.FormatAccountScopes(recorded.MatchFilter.AccountScopes), " "))
			require.NoError(parsed.Err())
			targets, err := e.GetDeletionTargetsBySearch(t.Context(), parsed, query.MessageFilter{}, query.DeletionSearchFast)
			require.NoError(err)
			require.Len(targets, 1)
			assert.Equal(tc.want, targets[0].SourceMessageID)
		})
	}
}

func TestSelectedAggregateDeletionPreservesVirtualAccountScope(t *testing.T) {
	setup := require.New(t)
	f := storetest.New(t)
	setup.NoError(f.Store.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	setup.NoError(f.Store.AddAccountIdentity(f.Source.ID, "mask@example.org", "fastmail-masked-email"))
	sender := f.EnsureParticipant("sender@example.org", "Sender", "example.org")
	for _, tc := range []struct{ key, header string }{
		{"work", "work@example.org"}, {"mask", "mask@example.org"}, {"ambiguous", "work@example.org, mask@example.org"},
	} {
		id := f.CreateMessage(tc.key)
		setup.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		setup.NoError(f.Store.UpsertMessageRaw(id, []byte("From: sender@example.org\r\nX-Delivered-To: "+tc.header+"\r\n\r\nbody")))
	}
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if f.Store.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(f.Store.DB(), dialect)
	for _, tc := range []struct {
		name  string
		scope search.AccountScope
		want  string
	}{
		{"identity", search.AccountScope{SourceID: &f.Source.ID, Addresses: []string{"work@example.org"}, Inbound: true}, "work"},
		{"group", search.AccountScope{SourceID: &f.Source.ID, Groups: []string{"fastmail-masked:" + f.Source.Identifier}}, "mask"},
		{"unattributed", search.AccountScope{SourceID: &f.Source.ID, Unattributed: true}, "ambiguous"},
	} {
		for _, drilled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/drilled=%t", tc.name, drilled), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				dctx := DeletionContext{
					AggregateSelection: map[string]bool{"sender@example.org": true}, AggregateViewType: query.ViewSenders,
					AccountFilter: &f.Source.ID, MatchFilter: query.MessageFilter{AccountScopes: []search.AccountScope{tc.scope}},
				}
				if drilled {
					dctx.DrillFilter = &query.MessageFilter{Domain: "example.org"}
				}
				controller := NewActionController(e, t.TempDir(), nil)
				manifest, err := controller.StageForDeletion(dctx)
				require.NoError(err)
				assert.Equal([]string{tc.want}, manifest.GmailIDs, "selected sender staging must stay in the displayed virtual account")
			})
		}
	}
}

func TestVirtualAccountEmailTotalsExcludeCalendarEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("gcal", "work@example.org")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(src.ID, "work@example.org", "manual"))
	require.NoError(f.Store.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"work@example.org","account_email":"inbox@example.net"}`))
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "calendar", MessageType: "calendar_event"})
	require.NoError(err)
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if f.Store.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(f.Store.DB(), dialect)
	m := Model{mode: modeEmail, engine: e, sourceScope: virtualSourceScope(store.VirtualAccount{Key: "work", SourceID: src.ID, AccountAddress: "work@example.org"})}
	for _, want := range []int64{0, 1} {
		if want == 1 {
			_, err = f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "mail", MessageType: "email"}, RawMIME: []byte("To: work@example.org\r\n\r\nbody")})
			require.NoError(err)
		}
		msg, ok := m.loadStats()().(statsLoadedMsg)
		require.True(ok)
		require.NoError(msg.err)
		require.NotNil(msg.stats)
		assert.Equal(want, msg.stats.MessageCount)
		messages, err := e.ListMessages(t.Context(), emailScopedMessageFilter(m.currentSearchFilter()))
		require.NoError(err)
		assert.Len(messages, int(want), "totals match the displayed Email-mode list")
	}
}
