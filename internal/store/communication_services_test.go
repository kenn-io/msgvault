package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSeededServiceCatalogCoversTheRoadmapSet(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store

	services, err := st.ListCommunicationServicesContext(context.Background(), true)
	require.NoError(err)
	bySlug := make(map[string]store.CommunicationService, len(services))
	for _, service := range services {
		bySlug[service.Slug] = service
	}
	for _, slug := range []string{
		"whatsapp", "telegram", "facebook", "messenger", "instagram", "signal",
		"x", "discord", "slack", "linkedin", "sms", "rcs", "google-messages",
		"google-voice", "google-chat", "irc", "groupme", "imessage", "line",
		"bluesky", "matrix", "reddit", "kakaotalk", "wechat",
	} {
		service, ok := bySlug[slug]
		assert.True(ok, "seeded service %q must exist", slug)
		assert.True(service.IsSystem, "seeded service %q must be system-owned", slug)
		assert.True(service.IsActive, "seeded service %q must be active", slug)
	}
	assert.Equal(store.ScopePolicyRequired, bySlug["slack"].ScopePolicy)
	assert.Equal(store.NormalizationPhoneE164, bySlug["whatsapp"].Normalization)
	assert.Equal(store.NormalizationNone, bySlug["matrix"].Normalization)
}

func TestServiceAliasesResolveToOneCanonicalService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	for _, test := range []struct{ lookup, want string }{
		{"twitter", "x"}, {"X", "x"}, {"gmessages", "google-messages"},
		{"bsky", "bluesky"}, {"bluesky", "bluesky"},
	} {
		service, err := st.ResolveCommunicationServiceContext(ctx, test.lookup)
		require.NoError(err, test.lookup)
		assert.Equal(test.want, service.Slug, test.lookup)
	}
	_, err := st.ResolveCommunicationServiceContext(ctx, "no-such-bridge")
	require.ErrorIs(err, store.ErrServiceNotFound)
}

func TestUnknownServiceIsRegisteredWithoutASchemaMigration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	created, isNew, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{
		Slug: "example-bridge", DisplayLabel: "Example Bridge",
		Aliases: []string{"examplebridge"}, ScopePolicy: store.ScopePolicyOptional,
		Normalization: store.NormalizationLower, NormalizationVersion: 1,
	})
	require.NoError(err)
	assert.True(isNew)
	assert.False(created.IsSystem)

	again, isNew, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{
		Slug: "example-bridge", DisplayLabel: "Example Bridge",
		ScopePolicy:   store.ScopePolicyOptional,
		Normalization: store.NormalizationLower, NormalizationVersion: 1,
	})
	require.NoError(err)
	assert.False(isNew)
	assert.Equal(created.ID, again.ID)
	resolved, err := st.ResolveCommunicationServiceContext(ctx, "examplebridge")
	require.NoError(err)
	assert.Equal("example-bridge", resolved.Slug)
}

func TestServiceSeedIsIdempotentAndPreservesUserEdits(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	service, err := st.ResolveCommunicationServiceContext(ctx, "slack")
	require.NoError(err)
	renamed, err := st.UpdateCommunicationServiceContext(ctx, service.ID, store.CommunicationServiceInput{
		Slug: "slack", DisplayLabel: "Work Chat",
		ScopePolicy: store.ScopePolicyRequired, DefaultScopeKind: new("workspace"),
		Normalization: store.NormalizationLower, NormalizationVersion: 1,
	})
	require.NoError(err)
	assert.Equal("Work Chat", renamed.DisplayLabel)
	require.NoError(st.InitSchema())
	after, err := st.ResolveCommunicationServiceContext(ctx, "slack")
	require.NoError(err)
	assert.Equal("Work Chat", after.DisplayLabel)
	assert.Equal(service.ID, after.ID)
}

func TestServiceAliasCannotBeStolenFromAnotherService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	_, _, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{
		Slug: "other-bridge", DisplayLabel: "Other Bridge", Aliases: []string{"twitter"},
		ScopePolicy: store.ScopePolicyNone, Normalization: store.NormalizationLower,
		NormalizationVersion: 1,
	})
	require.Error(err)
	require.ErrorIs(err, store.ErrServiceAliasConflict)
	resolved, err := st.ResolveCommunicationServiceContext(ctx, "twitter")
	require.NoError(err)
	assert.Equal("x", resolved.Slug)
}

func TestNormalizeServiceValuePerStrategy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	tests := []struct {
		service     string
		addressKind store.ContactAddressKind
		raw, want   string
		wantErr     bool
	}{
		{"whatsapp", store.ContactAddressPhone, "+1 (202) 555-0123", "+12025550123", false},
		{"signal", store.ContactAddressPhone, "202-555-0123", "+12025550123", false},
		{"whatsapp", store.ContactAddressPhone, "alice@example.com", "", true},
		{"x", store.ContactAddressUsername, "@Alice", "alice", false},
		{"bluesky", store.ContactAddressUsername, "@Alice.bsky.social", "alice.bsky.social", false},
		{"discord", store.ContactAddressUsername, "Alice", "alice", false},
		{"google-chat", store.ContactAddressEmail, "Alice@Example.com", "alice@example.com", false},
		{"matrix", store.ContactAddressUsername, "@Alice:example.org", "@Alice:example.org", false},
		{"imessage", store.ContactAddressEmail, "Alice@Example.com", "alice@example.com", false},
		{"imessage", store.ContactAddressPhone, "202-555-0123", "+12025550123", false},
	}
	for _, test := range tests {
		service, err := st.ResolveCommunicationServiceContext(ctx, test.service)
		require.NoError(err)
		got, err := store.NormalizeServiceValue(service, test.addressKind, test.raw)
		if test.wantErr {
			require.ErrorIs(err, store.ErrNormalizationRejected)
			continue
		}
		require.NoError(err)
		assert.Equal(test.want, got)
	}
}

func TestValidateServiceScopeFollowsScopePolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	slack, err := st.ResolveCommunicationServiceContext(ctx, "slack")
	require.NoError(err)
	whatsapp, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
	require.NoError(err)
	require.ErrorIs(store.ValidateServiceScope(slack, nil, nil), store.ErrServiceScopeRequired)
	assert.NoError(store.ValidateServiceScope(slack, new("workspace"), new("T0EXAMPLE")))
	assert.NoError(store.ValidateServiceScope(whatsapp, nil, nil))
	require.ErrorIs(
		store.ValidateServiceScope(whatsapp, new("workspace"), new("T0EXAMPLE")),
		store.ErrServiceScopeForbidden,
	)
}
