package carddav

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func seedResolvedConnectionConflicts(t *testing.T) (*store.Store, map[string]int64) {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	conflicts := map[string]int64{}
	for _, name := range []string{"default", "work"} {
		account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: name, BaseURL: "https://dav.example.test", Username: "synthetic-user", PrincipalURL: "https://dav.example.test/principal/", HomeURL: "https://dav.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://dav.example.test/books/shared/", DisplayName: "Synthetic Book", CanCreate: new(true)}}})
		requirements.NoError(err)
		requirements.Len(books, 1)
		book := books[0]
		requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true}))
		books, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
		requirements.NoError(err)
		requirements.Len(books, 1)
		book = books[0]
		body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-%s\r\nFN:Synthetic Cleanup\r\nEMAIL:cleanup-%s@example.test\r\nEND:VCARD\r\n", name, name))
		remote, err := parseRemoteResource(book.CanonicalURL+"cleanup.vcf", `"base"`, body)
		requirements.NoError(err)
		_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
		requirements.NoError(err)
		mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
		requirements.NoError(err)
		requirements.NotEmpty(mapping.LocalHash)
		requirements.NotNil(mapping.PersonID)
		snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
		requirements.NoError(err)
		conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{AddressBookID: book.ID, Href: mapping.Href, ExpectedMappingRevision: mapping.MappingRevision, BaseLocalHash: mapping.LocalHash, LocalHash: snapshot.Fingerprint, BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag, RemoteETag: `"changed"`, LocalBody: body, RemoteBody: body})
		requirements.NoError(err)
		remote.RemoteETag = `"changed"`
		_, err = st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, Remote: remote})
		requirements.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE carddav_conflicts SET resolved_at = ? WHERE id = ?`), time.Now().UTC().Add(-31*24*time.Hour), conflict.ID)
		requirements.NoError(err)
		// No subscribed books or remote work: Sync still runs native audit cleanup.
		requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{}))
		conflicts[name] = conflict.ID
	}
	return st, conflicts
}

func TestConnectionSyncKeepsForeignResolvedConflictAudit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, conflicts := seedResolvedConnectionConflicts(t)
	service := NewRemoteService(st, newMemoryRemote()).ForConnection("default", 1)
	result, err := service.Sync(t.Context(), SyncOptions{})
	requirements.NoError(err)
	assertions.Equal(0, result.Books)
	_, err = st.GetCardDAVConflictContext(t.Context(), conflicts["default"])
	requirements.ErrorIs(err, store.ErrCardDAVConflictNotFound)
	foreign, err := st.GetCardDAVConflictContext(t.Context(), conflicts["work"])
	requirements.NoError(err, "another connection's resolved audit must remain")
	assertions.Equal(conflicts["work"], foreign.ID)
	assertions.Equal(store.CardDAVConflictStatus("resolved"), foreign.Status)
}

func TestConnectionCleanupRejectsInvalidScopeAndStaleClient(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, conflicts := seedResolvedConnectionConflicts(t)
	for _, accountID := range []int64{store.AllCardDAVAccounts, -1} {
		removed, err := st.SweepResolvedCardDAVConflictsForAccountContext(t.Context(), time.Now(), accountID)
		requirements.Error(err)
		assertions.Zero(removed)
	}
	account, err := st.GetCardDAVAccountByNameContext(t.Context(), "default")
	requirements.NoError(err)
	requirements.NotNil(account)
	service := NewRemoteService(st, newMemoryRemote()).ForConnection("default", account.ConnectionGeneration)
	updated, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		ConnectionName: account.ConnectionName, BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL, CredentialsChanged: true,
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://dav.example.test/books/shared/", DisplayName: "Synthetic Book", CanCreate: new(true)}},
	})
	requirements.NoError(err)
	requirements.Equal(account.ConnectionGeneration+1, updated.ConnectionGeneration)
	_, err = service.Sync(t.Context(), SyncOptions{})
	requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
	removed, err := service.sweepResolvedConflicts(t.Context(), time.Now())
	requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
	assertions.Zero(removed)
	for _, name := range []string{"default", "work"} {
		conflict, err := st.GetCardDAVConflictContext(t.Context(), conflicts[name])
		requirements.NoError(err, "rejected cleanup must leave each account's audit intact")
		assertions.Equal(conflicts[name], conflict.ID)
		assertions.Equal(store.CardDAVConflictResolved, conflict.Status)
	}
}
