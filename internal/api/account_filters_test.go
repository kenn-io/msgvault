package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParseAccountScopes(t *testing.T) {
	assert := assert.New(t)
	request := func(values url.Values) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/v1/messages?"+values.Encode(), nil)
	}
	source := int64(7)
	scopes, err := parseAccountScopes(request(url.Values{
		"account_scopes": {`[{"source_id":7,"unattributed":true},{"addresses":["work@example.org"]}]`},
	}))
	require.NoError(t, err)
	assert.Equal([]search.AccountScope{
		{SourceID: &source, Unattributed: true},
		{Addresses: []string{"work@example.org"}},
	}, scopes)

	for name, values := range map[string]url.Values{
		"display name":   {"account_scopes": {`[{"addresses":["Name <a@example.org>"]}]`}},
		"unknown member": {"account_scopes": {`[{"groups":["x"]}]`}},
	} {
		_, err := parseAccountScopes(request(values))
		require.Error(t, err, name)
	}
}

func TestExploreAccountFilterDimension(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	source := int64(7)
	got, err := exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{store.VirtualIdentityKey(7, "work@example.org")}}})
	require.NoError(err)
	assert.Equal([]search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}, got.AccountScopes)

	got, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{store.VirtualUnattributedKey(7)}}})
	require.NoError(err)
	assert.Equal([]search.AccountScope{{SourceID: &source, Unattributed: true}}, got.AccountScopes)

	got, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{"Mask@Example.org"}}})
	require.NoError(err)
	assert.Equal([]search.AccountScope{{Addresses: []string{"mask@example.org"}}}, got.AccountScopes)

	_, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{"identity:7:!!"}}})
	require.Error(err)
	_, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{"a@example.org", "b@example.org"}}})
	require.Error(err)
}

func TestHandleCLIAccountsSurvivesCatalogFailure(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	_, err := st.GetOrCreateSource("mbox", "archive-1")
	require.NoError(err)
	// Hiding a table the catalog reads makes only the catalog fail.
	_, err = st.DB().Exec(`ALTER TABLE account_identities RENAME TO account_identities_off`)
	require.NoError(err)
	t.Cleanup(func() { _, _ = st.DB().Exec(`ALTER TABLE account_identities_off RENAME TO account_identities`) })

	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Accounts []struct {
			Email           string                 `json:"email"`
			VirtualAccounts []store.VirtualAccount `json:"virtual_accounts"`
		} `json:"accounts"`
		VirtualAccountsUnavailable bool `json:"virtual_accounts_unavailable"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.VirtualAccountsUnavailable)
	require.Len(resp.Accounts, 1)
	assert.Equal(t, "archive-1", resp.Accounts[0].Email)
	assert.Empty(t, resp.Accounts[0].VirtualAccounts)
}

func TestHandleCLIAccountsShowsNewIdentityDespiteCachedCatalog(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	src, err := st.GetOrCreateSource("mbox", "archive@example.net")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Thread")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv, SourceMessageID: "m1", MessageType: "email",
	})
	require.NoError(err)

	children := func() []string {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil))
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		var resp struct {
			Accounts []struct {
				VirtualAccounts []store.VirtualAccount `json:"virtual_accounts"`
			} `json:"accounts"`
		}
		require.NoError(json.NewDecoder(w.Body).Decode(&resp))
		require.Len(resp.Accounts, 1)
		var addresses []string
		for _, child := range resp.Accounts[0].VirtualAccounts {
			if !child.Unattributed {
				addresses = append(addresses, child.AccountAddress)
			}
		}
		return addresses
	}
	require.Empty(children())
	// The first catalog is still fresh, but confirming an address must show
	// up at once.
	require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
	assert.Equal(t, []string{"work@example.org"}, children())
}

// TestAgentAccountCatalogReadsReusedSourceIDFresh covers a SQLite source ID
// reused after a removal: the shared catalog still lists the removed source's
// addresses under that ID, and an agent granted the new source must not see
// them.
func TestAgentAccountCatalogReadsReusedSourceIDFresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "PostgreSQL identity columns never reuse an ID")
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}},
		Store:  st,
		Logger: testLogger(),
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	t.Cleanup(srv.agentGrants.Close)
	addSourceWithMail := func(identifier string) *store.Source {
		src, err := st.GetOrCreateSource("mbox", identifier)
		require.NoError(err)
		conv, err := st.EnsureConversation(src.ID, "thread", "Thread")
		require.NoError(err)
		_, err = st.UpsertMessage(&store.Message{
			SourceID: src.ID, ConversationID: conv, SourceMessageID: "m1", MessageType: "email",
		})
		require.NoError(err)
		return src
	}
	accounts := func(header, value string) string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil)
		req.Header.Set(header, value)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		return w.Body.String()
	}

	removed := addSourceWithMail("removed@example.net")
	require.NoError(st.AddAccountIdentity(removed.ID, "private@example.org", "manual"))
	require.Contains(accounts("Authorization", "Bearer owner"), "private@example.org", "the owner warms the shared catalog")
	require.NoError(st.RemoveSource(removed.ID))
	granted := addSourceWithMail("granted@example.net")
	require.Equal(removed.ID, granted.ID, "SQLite reuses the removed source's ID")

	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead},
		[]agentgrant.SourceRef{{ID: granted.ID, Type: granted.SourceType, Identifier: granted.Identifier}}, time.Time{})
	require.NoError(err)
	body := accounts(apiprotocol.AgentTokenHeader, token)
	assert.Contains(body, "granted@example.net")
	assert.NotContains(body, "private@example.org", "an agent never sees a removed source's addresses")
}

// TestAgentAccountCatalogUsesAuthorizationSnapshot replaces a granted source
// while an agent request is between its authorization and its catalog read.
// The catalog must come from the request's snapshot, or the replacement's
// addresses would appear under the reused source ID.
func TestAgentAccountCatalogUsesAuthorizationSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "PostgreSQL identity columns never reuse an ID")
	st := testutil.NewTestStore(t)
	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := &agentReadGateStore{Store: st, onSources: func() {
		once.Do(func() {
			close(paused)
			<-resume
		})
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}},
		Store:  gate,
		Logger: testLogger(),
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	t.Cleanup(srv.agentGrants.Close)
	addSourceWithMail := func(identifier string) *store.Source {
		src, err := st.GetOrCreateSource("mbox", identifier)
		require.NoError(err)
		conv, err := st.EnsureConversation(src.ID, "thread", "Thread")
		require.NoError(err)
		_, err = st.UpsertMessage(&store.Message{
			SourceID: src.ID, ConversationID: conv, SourceMessageID: "m1", MessageType: "email",
		})
		require.NoError(err)
		return src
	}
	granted := addSourceWithMail("granted@example.net")
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead},
		[]agentgrant.SourceRef{{ID: granted.ID, Type: granted.SourceType, Identifier: granted.Identifier}}, time.Time{})
	require.NoError(err)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		done <- w
	}()
	<-paused
	require.NoError(st.RemoveSource(granted.ID))
	replacement := addSourceWithMail("replacement@example.net")
	require.Equal(granted.ID, replacement.ID, "SQLite reuses the removed source's ID")
	require.NoError(st.AddAccountIdentity(replacement.ID, "private@example.org", "manual"))
	close(resume)

	w := <-done
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(w.Body.String(), "private@example.org", "the catalog read sees the authorization snapshot")
}
