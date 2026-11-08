package api

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

func TestICloudRelocationAdmissionConjunctionProperty(t *testing.T) {
	f := savedICloudAccountFixture(t)
	books, err := f.controller.scopedStoredBooks(t.Context())
	require.NoError(t, err)
	require.Len(t, books, 1)
	hegel.Test(t, func(ht *hegel.T) {
		write, subscribed, lookup := hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans())
		rolesErr := f.controller.globalOperations().SetBookRoles(ht.Context(), books[0].ID, carddav.BookRoles{WriteTarget: write, Subscribed: subscribed, LookupSource: lookup})
		if write && !subscribed {
			require.ErrorIs(ht, rolesErr, store.ErrCardDAVWriteTargetSubscribed)
			return
		}
		require.NoError(ht, rolesErr)
		known := hegel.Draw(ht, hegel.Booleans())
		create := known && hegel.Draw(ht, hegel.Booleans())
		update := known && hegel.Draw(ht, hegel.Booleans())
		remove := known && hegel.Draw(ht, hegel.Booleans())
		discovery := iCloudDiscoveryFixture("p43")
		discovery.Books[0].Capabilities = carddav.BookCapabilities{CreateKnown: known, Create: create, UpdateKnown: known, Update: update, DeleteKnown: known, Delete: remove}
		want := write && subscribed && lookup && known && create && update && remove
		assert.Equal(ht, want, f.controller.admitICloudRelocation(ht.Context(), discovery) == nil)
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}

func TestICloudStartupRequiresExactStoredGenerationProperty(t *testing.T) {
	f := savedICloudAccountFixture(t)
	credential, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		generation := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		changed := credential
		changed.ConnectionGeneration = generation
		require.NoError(ht, carddav.SaveCredential(f.controller.cfg.TokensDir(), changed))
		controller, loadErr := NewCardDAVController(f.controller.cfg, f.controller.store, slog.New(slog.DiscardHandler))
		require.NoError(ht, loadErr)
		assert.Equal(ht, generation == credential.ConnectionGeneration, controller.Current() != nil)
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}

func TestICloudPublicBootstrapCannotImplicitlyReusePartitionPassword(t *testing.T) {
	f := savedICloudAccountFixture(t)
	req := iCloudAccountRequest()
	req.Password = ""
	_, err := f.controller.Save(t.Context(), req)
	require.Error(t, err)
	assert.Empty(t, f.discovered)
}

func TestLegacyPartitionSetupDoesNotEnablePreflight(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newICloudAccountFixture(t)
	req := iCloudAccountRequest()
	req.BaseURL = "https://p42-contacts.icloud.com"
	_, err := f.controller.Save(t.Context(), req)
	require.NoError(err)
	f.discovered = nil
	_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.NoError(err)
	assert.Empty(f.discovered)
	assert.Equal(1, f.syncCalls)
}

func seedICloudOwnedPublication(t *testing.T, st *store.Store, book store.CardDAVAddressBook) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?) RETURNING id`), "icloud-synthetic-publication", "Synthetic Contact").Scan(&id))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_publications (person_id, desired, address_book_id, href) VALUES (?, TRUE, ?, ?)`), id, book.ID, book.CanonicalURL+"synthetic.vcf")
	require.NoError(t, err)
	return id
}

func TestICloudSamePartitionReconnectPreservesOwnedPublication(t *testing.T) {
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	books, err := f.controller.scopedStoredBooks(t.Context())
	require.NoError(err)
	id := seedICloudOwnedPublication(t, f.controller.store, books[0])
	_, err = f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	publication, err := f.controller.store.GetCardDAVPublicationContext(t.Context(), id)
	require.NoError(err)
	require.NotNil(publication)
	assert.Equal(t, books[0].ID, publication.AddressBookID)
}

func TestICloudPublicationInsertedAtPersistenceBlocksRelocationAndRollsBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	books, err := f.controller.scopedStoredBooks(t.Context())
	require.NoError(err)
	before, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	actualPersist := f.controller.persistDiscovery
	f.controller.persistDiscovery = func(ctx context.Context, service cardDAVCandidate, base, user string, discovery carddav.Discovery, changed bool) error {
		seedICloudOwnedPublication(t, f.controller.store, books[0])
		return actualPersist(ctx, service, base, user, discovery, changed)
	}
	f.partition, f.partitionErr = "p43", &carddav.StatusError{StatusCode: http.StatusGone}
	_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(err, carddav.ErrICloudReconnectRequired)
	after, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	assert.Equal(before, after)
	assert.Equal(before.BaseURL, f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Zero(f.syncCalls)
}

func TestICloudRetryGateSurvivesRuntimeReload(t *testing.T) {
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	require.NoError(f.controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(time.Hour), store.DefaultCardDAVAccountID))
	controller, err := NewCardDAVController(f.controller.cfg, f.controller.store, slog.New(slog.DiscardHandler))
	require.NoError(err)
	var factories int
	controller.factory = func(*store.Store, config.CardDAVConfig, string) (cardDAVCandidate, error) {
		factories++
		return nil, errors.New("unexpected synthetic runtime rebuild")
	}
	_, err = controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Zero(t, factories)
}
