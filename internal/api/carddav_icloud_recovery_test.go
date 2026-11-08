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

func TestICloudPreflightMovesPartitionBeforeOneFullSync(t *testing.T) {
	assert := assert.New(t)
	f := savedICloudAccountFixture(t)
	f.partition = "p43"
	f.partitionErr = &carddav.StatusError{StatusCode: http.StatusGone}
	var runID int64
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{Trigger: store.CardDAVSyncTriggerScheduled, OnRunStarted: func(id int64) { runID = id }})
	require.NoError(t, err)
	assert.Equal("https://p43-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Equal([]string{"https://p42-contacts.icloud.com", carddav.ICloudDiscoveryURL}, f.discovered)
	assert.Equal(1, f.syncCalls)
	assert.True(f.syncOptions.Full)
	assert.Equal(store.CardDAVSyncTriggerScheduled, f.syncOptions.Trigger)
	assert.Equal(int64(123), runID)
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
	require.NoError(t, err)
	assert.Equal(t, "https://p43-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
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
