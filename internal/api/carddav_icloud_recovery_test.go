package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func savedICloudAccountFixture(t *testing.T) *iCloudAccountFixture {
	t.Helper()
	f := newICloudAccountFixture(t)
	_, err := f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(t, err)
	f.discovered = nil
	return f
}

func TestICloudReconnectPreservesIdentityAcrossUsernameCase(t *testing.T) {
	for _, tc := range []struct{ name, baseURL string }{
		{"icloud setup", carddav.ICloudDiscoveryURL},
		{"legacy partition setup", "https://p42-contacts.icloud.com"},
		{"legacy partition root slash", "https://p42-contacts.icloud.com/"},
		{"legacy partition explicit port", "https://P42-contacts.icloud.com:443/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newICloudAccountFixture(t)
			req := iCloudAccountRequest()
			req.Username = "Synthetic@Example.net"
			req.BaseURL = tc.baseURL
			_, err := f.controller.Save(t.Context(), req)
			require.NoError(err)
			books, err := f.controller.scopedStoredBooks(t.Context())
			require.NoError(err)
			require.Len(books, 1)
			account, err := f.controller.store.GetCardDAVAccountByNameContext(t.Context(), config.DefaultCardDAVConnection)
			require.NoError(err)
			require.NotNil(account)
			href := books[0].CanonicalURL + "synthetic.vcf"
			resource := store.CardDAVRemoteResource{Href: href, RemoteUID: "synthetic-uid", RemoteETag: `"one"`,
				RemoteBody:   []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-uid\r\nFN:Synthetic Contact\r\nEND:VCARD\r\n"),
				SemanticHash: "synthetic-hash", DisplayName: "Synthetic Contact"}
			_, err = f.controller.store.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision,
				Upserts: []store.CardDAVRemoteResource{resource},
			})
			require.NoError(err)
			before, err := f.controller.store.GetCardDAVResourceContext(t.Context(), books[0].ID, href)
			require.NoError(err)
			require.NotNil(before.PersonID)
			personID := seedICloudOwnedPublication(t, f.controller.store, books[0])
			_, err = f.controller.Save(t.Context(), iCloudAccountRequest())
			require.NoError(err)
			after, err := f.controller.scopedStoredBooks(t.Context())
			require.NoError(err)
			require.Len(after, 1)
			assert.Equal(books[0].ID, after[0].ID)
			retained, err := f.controller.store.GetCardDAVResourceContext(t.Context(), books[0].ID, href)
			require.NoError(err)
			assert.Equal(before.PersonID, retained.PersonID)
			publication, err := f.controller.store.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			require.NotNil(publication)
			assert.Equal(books[0].ID, publication.AddressBookID)
		})
	}
}

func TestICloudRecoveryPreservesImportedIdentity(t *testing.T) {
	for _, withOtherAccount := range []bool{false, true} {
		name := "single account"
		if withOtherAccount {
			name = "two accounts"
		}
		t.Run(name, func(t *testing.T) {
			testICloudRecoveryPreservesImportedIdentity(t, withOtherAccount)
		})
	}
}

func testICloudRecoveryPreservesImportedIdentity(t *testing.T, withOtherAccount bool) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	ctx := t.Context()
	st := f.controller.store
	account, err := st.GetCardDAVAccountByNameContext(ctx, config.DefaultCardDAVConnection)
	require.NoError(err)
	require.NotNil(account)
	books, err := f.controller.scopedStoredBooks(ctx)
	require.NoError(err)
	require.Len(books, 1)
	book := books[0]
	uid := "d11a2f73-f058-4b0e-bd31-03b473ff31c9"
	href := book.CanonicalURL + "synthetic.vcf"
	resource := store.CardDAVRemoteResource{
		Href: href, RemoteUID: uid, RemoteETag: `"one"`, SemanticHash: "synthetic-hash",
		RemoteBody:  []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:Synthetic Contact\r\nEND:VCARD\r\n"),
		DisplayName: "Synthetic Contact",
	}
	_, err = st.ApplyCardDAVSyncPlanContext(ctx, store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{resource},
	})
	require.NoError(err)
	before, err := st.GetCardDAVResourceContext(ctx, book.ID, href)
	require.NoError(err)
	require.NotNil(before.PersonID)
	var personUID string
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_uid FROM persons WHERE id = ?`), *before.PersonID).Scan(&personUID))
	if withOtherAccount {
		_, _, err = st.ReplaceCardDAVDiscoveryContext(ctx, store.CardDAVDiscoveryInput{
			ConnectionName: "other", BaseURL: "https://contacts.example.test", Username: "other@example.test",
			PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
		})
		require.NoError(err)
	}
	f.partition, f.partitionErr = "p43", &carddav.StatusError{StatusCode: http.StatusGone}
	_, err = f.controller.Current().Sync(ctx, carddav.SyncOptions{})
	require.ErrorIs(err, carddav.ErrICloudReconnectRequired)
	assert.Zero(f.syncCalls)
	after, err := st.GetCardDAVResourceContext(ctx, book.ID, href)
	require.NoError(err)
	assert.Equal(before.PersonID, after.PersonID)
	assert.Equal(before.RemoteUID, after.RemoteUID)
	var afterUID string
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_uid FROM persons WHERE id = ?`), *before.PersonID).Scan(&afterUID))
	assert.Equal(personUID, afterUID)
	books, err = f.controller.scopedStoredBooks(ctx)
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(book.ID, books[0].ID)
	assert.True(books[0].IsSubscribed)
	assert.True(books[0].IsWriteTarget)
}

func TestICloudRecoveryPreservesBookRolesWithAnotherAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	ctx := t.Context()
	books, err := f.controller.scopedStoredBooks(ctx)
	require.NoError(err)
	require.Len(books, 1)
	before := books[0]
	_, _, err = f.controller.store.ReplaceCardDAVDiscoveryContext(ctx, store.CardDAVDiscoveryInput{
		ConnectionName: "other", BaseURL: "https://contacts.example.test", Username: "other@example.test",
		PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
	})
	require.NoError(err)
	f.partition, f.partitionErr = "p43", &carddav.StatusError{StatusCode: http.StatusGone}
	_, err = f.controller.Current().Sync(ctx, carddav.SyncOptions{})
	require.ErrorIs(err, carddav.ErrICloudReconnectRequired)
	after, err := f.controller.scopedStoredBooks(ctx)
	require.NoError(err)
	require.Len(after, 1)
	assert.Equal(before.ID, after[0].ID)
	assert.True(after[0].IsSubscribed)
	assert.True(after[0].IsWriteTarget)
	assert.True(after[0].IsLookupSource)
}

func TestICloudPreflightRefusesMovedPartition(t *testing.T) {
	assert := assert.New(t)
	f := savedICloudAccountFixture(t)
	f.partition = "p43"
	f.partitionErr = &carddav.StatusError{StatusCode: http.StatusGone}
	var runID int64
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{Trigger: store.CardDAVSyncTriggerScheduled, OnRunStarted: func(id int64) { runID = id }})
	require.ErrorIs(t, err, carddav.ErrICloudReconnectRequired)
	assert.Equal("https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Equal([]string{"https://p42-contacts.icloud.com", carddav.ICloudDiscoveryURL}, f.discovered)
	assert.Zero(f.syncCalls)
	assert.Zero(runID)
}

func TestICloudNeverRediscoversAfterUnderlyingSyncFailure(t *testing.T) {
	f := savedICloudAccountFixture(t)
	f.syncErr = &carddav.StatusError{StatusCode: http.StatusGone}
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.Error(t, err)
	assert.Equal(t, []string{"https://p42-contacts.icloud.com"}, f.discovered)
	assert.Equal(t, 1, f.syncCalls, "zero counters do not authorize replay")
}

func TestICloudRecoveryRefusesCustomBookChoices(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	books, err := f.controller.scopedStoredBooks(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	require.NoError(f.controller.globalOperations().SetBookRoles(t.Context(), books[0].ID, carddav.BookRoles{WriteTarget: true, Subscribed: true, LookupSource: false}))
	f.partition = "p43"
	f.partitionErr = &carddav.StatusError{StatusCode: http.StatusGone}
	_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.Error(err)
	assert.Contains(err.Error(), "iCloud")
	assert.Equal("https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Zero(f.syncCalls)
	after, err := f.controller.scopedStoredBooks(t.Context())
	require.NoError(err)
	assert.Equal(books[0].ID, after[0].ID)
	assert.False(after[0].IsLookupSource)
}

func TestICloudPreflightHonorsAndPersistsRetryGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	f.partitionErr = &carddav.StatusError{StatusCode: http.StatusNotFound, RetryAfter: time.Hour}
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.Error(err)
	assert.Equal([]string{"https://p42-contacts.icloud.com"}, f.discovered)
	gate, err := f.controller.store.GetCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	require.NotNil(gate)
	f.discovered = nil
	_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Empty(f.discovered)
	assert.Zero(f.syncCalls)
}

func TestICloudUnchangedPartitionDoesNotReplaceFailedConnection(t *testing.T) {
	f := savedICloudAccountFixture(t)
	f.partitionErr = &carddav.StatusError{StatusCode: http.StatusGone}
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.Error(t, err)
	assert.Equal(t, "https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Zero(t, f.syncCalls)
}

func TestICloudPreflightDoesNotBootstrapUnsafeOrAuthenticationFailures(t *testing.T) {
	assert := assert.New(t)
	for _, failure := range []error{carddav.ErrUnsafeTarget, carddav.ErrUnsafeRedirect, context.Canceled, context.DeadlineExceeded, &carddav.StatusError{StatusCode: http.StatusUnauthorized}, &carddav.StatusError{StatusCode: http.StatusForbidden}, &carddav.StatusError{StatusCode: http.StatusTooManyRequests}, errors.New("sentinel-private-path")} {
		f := savedICloudAccountFixture(t)
		f.partitionErr = failure
		_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
		require.Error(t, err)
		assert.Equal([]string{"https://p42-contacts.icloud.com"}, f.discovered)
		assert.NotContains(err.Error(), "sentinel")
		assert.Zero(f.syncCalls)
	}
}

func TestICloudDialFailureMayRediscover(t *testing.T) {
	f := savedICloudAccountFixture(t)
	f.partition = "p43"
	f.partitionErr = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic connection failure")}
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(t, err, carddav.ErrICloudReconnectRequired)
	assert.Equal(t, "https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
}

func TestICloudHealthyPreflightDoesNotPersistDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	before, err := f.controller.store.GetCardDAVAccountByNameContext(t.Context(), config.DefaultCardDAVConnection)
	require.NoError(err)
	_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.NoError(err)
	after, err := f.controller.store.GetCardDAVAccountByNameContext(t.Context(), config.DefaultCardDAVConnection)
	require.NoError(err)
	assert.Equal(before.DiscoveryRevision, after.DiscoveryRevision)
	assert.Equal([]string{"https://p42-contacts.icloud.com"}, f.discovered)
}

func TestICloudRoleEditDuringRediscoveryWinsBeforeRelocationAdmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	books, err := f.controller.scopedStoredBooks(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	f.partition, f.partitionErr = "p43", &carddav.StatusError{StatusCode: http.StatusGone}
	entered, release := make(chan struct{}), make(chan struct{})
	f.discoverHook = func(base string) {
		if base == carddav.ICloudDiscoveryURL {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { _, syncErr := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{}); done <- syncErr }()
	select {
	case <-entered:
	case syncErr := <-done:
		require.FailNow("sync returned before bootstrap", "%v", syncErr)
	}
	err = f.controller.globalOperations().SetBookRoles(t.Context(), books[0].ID, carddav.BookRoles{WriteTarget: true, Subscribed: true, LookupSource: false})
	close(release)
	require.NoError(err)
	require.ErrorIs(<-done, carddav.ErrICloudReconnectRequired)
	assert.Equal("https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Zero(f.syncCalls)
}

func TestICloudConcurrentPasswordRotationAbortsRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	f.partition, f.partitionErr = "p43", &carddav.StatusError{StatusCode: http.StatusGone}
	f.discoverHook = func(base string) {
		if base != carddav.ICloudDiscoveryURL {
			return
		}
		credential, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
		require.NoError(err)
		credential.Password = "rotated-synthetic-password"
		require.NoError(carddav.SaveCredential(f.controller.cfg.TokensDir(), credential))
	}
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(err, carddav.ErrICloudReconnectRequired)
	credential, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	assert.Equal("rotated-synthetic-password", credential.Password)
	assert.Equal("https://p42-contacts.icloud.com", credential.BaseURL)
	assert.Zero(f.syncCalls)
}

func TestICloudRetainedSchedulerWrapperUsesLatestService(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	retained := f.controller.Current()
	f.partition = "p43"
	_, err := f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	f.discovered = nil
	_, err = retained.Sync(t.Context(), carddav.SyncOptions{Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)
	assert.Equal([]string{"https://p43-contacts.icloud.com"}, f.discovered)
	assert.Equal(1, f.syncCalls)
}

func TestICloudBlockedRecoveryHasSafeSingleAndAggregateErrors(t *testing.T) {
	assert := assert.New(t)
	err := errors.Join(carddav.ErrICloudReconnectRequired, errors.New("sentinel-private-path"))
	w := httptest.NewRecorder()
	(&Server{}).writeCardDAVOperationError(w, err, "generic")
	assert.Equal(http.StatusConflict, w.Code)
	assert.Contains(w.Body.String(), "add-carddav --icloud")
	assert.NotContains(w.Body.String(), "sentinel")
	code, message := carddav.SyncFailure(err)
	assert.Equal("upstream_failed", code)
	assert.Equal(carddav.ErrICloudReconnectRequired.Error(), message)
}
