package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailpush"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

type gmailPushTestClient struct {
	profile              *gmail.Profile
	profileErr, watchErr error
	watches              int
	closed               int
	events               []string
}

func (c *gmailPushTestClient) GetProfile(context.Context) (*gmail.Profile, error) {
	c.events = append(c.events, "profile")
	return c.profile, c.profileErr
}
func (c *gmailPushTestClient) Watch(context.Context, string) (*gmail.WatchResult, error) {
	c.watches++
	c.events = append(c.events, "watch")
	if c.watchErr != nil {
		return nil, c.watchErr
	}
	return &gmail.WatchResult{HistoryID: "999", Expiration: time.Now().Add(time.Hour)}, nil
}
func (c *gmailPushTestClient) Close() error { c.closed++; return nil }

type gmailPushTestSubscriber struct{}

func (gmailPushTestSubscriber) Validate(context.Context) error { return nil }
func (gmailPushTestSubscriber) Pull(ctx context.Context) ([]emailpush.Delivery, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (gmailPushTestSubscriber) Acknowledge(context.Context, []string) error { return nil }

func gmailPushTestConfig() config.GmailPushConfig {
	return config.GmailPushConfig{SourceID: 1, Topic: "projects/example/topics/mail", Subscription: "projects/example/subscriptions/mail", CredentialsFile: "/synthetic/subscriber.json"}
}

func TestGmailPushBindingProfileBeforeWatchAndCatchup(t *testing.T) {
	requirements := require.New(t)
	src := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com", SyncCursor: sql.NullString{String: "37", Valid: true}}
	client := &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: src.Identifier}}
	var keys []string
	listener, err := newBoundGmailPushListener(src, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return src, nil }, func(_ context.Context, key string) error {
		client.events = append(client.events, "wake")
		keys = append(keys, key)
		return nil
	}, false, nil)
	requirements.NoError(err)
	requirements.ErrorIs(listener.Reconcile(t.Context()), emailpush.ErrSourceUnavailable)
	requirements.Empty(keys)
	watch, err := listener.Watch(t.Context())
	requirements.NoError(err)
	requirements.Equal(src.Identifier, watch.EmailAddress)
	requirements.NoError(listener.Reconcile(t.Context()))
	requirements.Equal([]string{"profile", "watch", "wake"}, client.events)
	requirements.Equal([]string{src.Identifier}, keys)
	requirements.Equal("37", src.SyncCursor.String)
}

func TestGmailPushBindingGmailAliasProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		source := &store.Source{ID: 1, SourceType: "gmail", Identifier: fmt.Sprintf("Synthetic.User+archive%d@googlemail.com", suffix)}
		client := &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: "syntheticuser@gmail.com"}}
		key := ""
		listener, err := newBoundGmailPushListener(source, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return source, nil }, func(_ context.Context, k string) error { key = k; return nil }, false, nil)
		require.NoError(ht, err)
		watch, err := listener.Watch(context.Background())
		require.NoError(ht, err)
		assert.Equal(ht, "syntheticuser@gmail.com", watch.EmailAddress)
		require.NoError(ht, listener.Reconcile(context.Background()))
		assert.Equal(ht, source.Identifier, key)
	})
}

func TestGmailPushBindingRejectsForeignProfileProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		source := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com"}
		client := &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: fmt.Sprintf("foreign%d@example.com", suffix)}}
		wakes := 0
		listener, err := newBoundGmailPushListener(source, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return source, nil }, func(context.Context, string) error { wakes++; return nil }, false, nil)
		require.NoError(ht, err)
		_, err = listener.Watch(context.Background())
		require.ErrorIs(ht, err, emailpush.ErrSourceUnavailable)
		assert.NotContains(ht, err.Error(), client.profile.EmailAddress)
		assert.Zero(ht, client.watches)
		require.ErrorIs(ht, listener.Reconcile(context.Background()), emailpush.ErrSourceUnavailable)
		assert.Zero(ht, wakes)
	})
}

func TestGmailPushBindingRechecksAfterReceipt(t *testing.T) {
	requirements := require.New(t)
	original := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com"}
	current := original
	client := &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: original.Identifier}}
	listener, err := newBoundGmailPushListener(original, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return current, nil }, func(context.Context, string) error {
		current = &store.Source{ID: 2, SourceType: "gmail", Identifier: original.Identifier}
		return nil
	}, false, nil)
	requirements.NoError(err)
	_, err = listener.Watch(t.Context())
	requirements.NoError(err)
	requirements.ErrorIs(listener.Reconcile(t.Context()), emailpush.ErrSourceUnavailable)
	requirements.ErrorIs(listener.ValidateSource(t.Context()), emailpush.ErrSourceUnavailable)
}

func TestGmailPushBindingTokenFailurePreventsWake(t *testing.T) {
	requirements := require.New(t)
	src := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com"}
	client := &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: src.Identifier}}
	tokenValid := true
	wakes := 0
	listener, err := newBoundGmailPushListener(src, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) {
		if !tokenValid {
			return nil, errEmailPushBinding
		}
		return src, nil
	}, func(context.Context, string) error { wakes++; return nil }, false, nil)
	requirements.NoError(err)
	_, err = listener.Watch(t.Context())
	requirements.NoError(err)
	tokenValid = false
	requirements.ErrorIs(listener.Reconcile(t.Context()), emailpush.ErrSourceUnavailable)
	requirements.Zero(wakes)
}

func TestGmailPushBindingRejectsRetiredSource(t *testing.T) {
	for _, kind := range []string{"history", "merged", "type", "identifier"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			src := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com"}
			client := &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: src.Identifier}}
			current := *src
			listener, err := newBoundGmailPushListener(src, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return &current, nil }, func(context.Context, string) error { return nil }, false, nil)
			requirements.NoError(err)
			switch kind {
			case "history":
				current.HistoryOnly = true
			case "merged":
				current.MergedIntoSourceID = 2
			case "type":
				current.SourceType = "imap"
			case "identifier":
				current.Identifier = "replacement@example.com"
			}
			requirements.ErrorIs(listener.ValidateSource(t.Context()), emailpush.ErrSourceUnavailable)
			_, err = listener.Watch(t.Context())
			requirements.ErrorIs(err, emailpush.ErrSourceUnavailable)
			requirements.Empty(client.events)
		})
	}
}

func TestGmailPushBindingCredentialFailureIsRedacted(t *testing.T) {
	requirements := require.New(t)
	src := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com"}
	client := &gmailPushTestClient{profileErr: provideridentity.NewGmailCredentialError(provideridentity.GmailAuthorizationRevoked, errors.New("synthetic-private-token-marker"))}
	listener, err := newBoundGmailPushListener(src, gmailPushTestConfig(), client, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return src, nil }, func(context.Context, string) error { return nil }, false, nil)
	requirements.NoError(err)
	_, err = listener.Watch(t.Context())
	requirements.Error(err)
	requirements.NotContains(err.Error(), "synthetic-private-token-marker")
	requirements.True(gmailPushPermanentFailure(err))
}

func TestGmailPushCanonicalSchedule(t *testing.T) {
	cfg := &config.Config{Accounts: []config.AccountSchedule{{Email: "synthetic@example.com", Enabled: true, Schedule: "0 * * * *"}}}
	require.True(t, gmailPushScheduleEnabled(cfg, "synthetic@example.com"))
	require.False(t, gmailPushScheduleEnabled(cfg, "alias@example.com"))
	cfg.Accounts = append(cfg.Accounts, cfg.Accounts[0])
	require.False(t, gmailPushScheduleEnabled(cfg, "synthetic@example.com"))
}
