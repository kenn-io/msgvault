package beeper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveBridgeServiceLadder(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		network   string
		userID    string
		wantSlug  string
	}{
		{
			name:      "account id is a seeded slug",
			accountID: "whatsapp", network: "WhatsApp",
			userID:   "@15550100001:beeper.local",
			wantSlug: "whatsapp",
		},
		{
			name:      "account id is a seeded alias",
			accountID: "twitter", network: "X",
			userID:   "@twitter_alice:beeper.local",
			wantSlug: "x",
		},
		{
			name:      "account id alias with a different spelling",
			accountID: "gmessages", network: "Google Messages",
			userID:   "@gmessages_15550100002:beeper.local",
			wantSlug: "google-messages",
		},
		{
			name:      "network label resolves when the account id does not",
			accountID: "acct-7", network: "Google Messages",
			userID:   "@acct7_alice:beeper.local",
			wantSlug: "google-messages",
		},
		{
			name:      "user id bridge prefix is the last rung",
			accountID: "acct-9", network: "",
			userID:   "@signal_alice-uuid:beeper.local",
			wantSlug: "signal",
		},
		{
			name:      "network label is normalized before lookup",
			accountID: "acct-11", network: "  KakaoTalk ",
			userID:   "@acct11_alice:beeper.local",
			wantSlug: "kakaotalk",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			resolver := newBridgeServiceResolver(st)

			service, ok, err := resolver.resolve(
				context.Background(), test.accountID, test.network, test.userID)
			require.NoError(err, "resolve")
			require.True(ok, "bridge must classify")
			assert.Equal(test.wantSlug, service.Slug)
			assert.True(service.IsSystem, "a seeded service must not be re-registered as user-owned")
		})
	}
}

func TestResolveUnknownBridgeRegistersItLosslessly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	resolver := newBridgeServiceResolver(st)

	service, ok, err := resolver.resolve(ctx, "example-bridge", "Example Bridge", "@example-bridge_alice:beeper.local")
	require.NoError(err, "resolve unknown bridge")
	require.True(ok, "an unknown bridge must be registered, never rejected")
	assert.Equal("example-bridge", service.Slug)
	assert.Equal("Example Bridge", service.DisplayLabel, "the source label is preserved verbatim")
	assert.False(service.IsSystem, "a runtime-registered bridge is not system-owned")
	assert.Equal(store.ScopePolicyOptional, service.ScopePolicy)
	assert.Equal(store.NormalizationNone, service.Normalization,
		"an unknown bridge must never have its values rewritten")
	assert.Empty(service.Aliases,
		"an unknown bridge claims no aliases, so it cannot steal one from a seeded service")

	// The registration is idempotent and cached: a second resolve neither
	// duplicates the row nor registers a second service.
	before, err := st.ListCommunicationServicesContext(ctx, true)
	require.NoError(err, "list services")
	again, ok, err := resolver.resolve(ctx, "example-bridge", "Example Bridge", "@example-bridge_bob:beeper.local")
	require.NoError(err, "second resolve")
	require.True(ok)
	assert.Equal(service.ID, again.ID)
	after, err := st.ListCommunicationServicesContext(ctx, true)
	require.NoError(err, "list services again")
	assert.Len(after, len(before), "resolving twice must not register a second service")
}

func TestResolveUnknownBridgeUsesUserIDPrefixWithOpaqueAccountID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	resolver := newBridgeServiceResolver(st)

	service, ok, err := resolver.resolve(
		context.Background(), "acct-opaque-7", "Mystery Bridge",
		"@mysterybridge_alice:beeper.local",
	)
	require.NoError(err, "resolve unknown bridge")
	require.True(ok, "the bridge prefix must register an unknown service")
	assert.Equal("mysterybridge", service.Slug)
	assert.Equal("Mystery Bridge", service.DisplayLabel)
}

func TestResolveUnknownBridgeCacheSeparatesUserIDPrefixes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	resolver := newBridgeServiceResolver(st)
	ctx := context.Background()

	first, ok, err := resolver.resolve(
		ctx, "acct-opaque-8", "Mystery Bridge", "@bridge-a_alice:beeper.local")
	require.NoError(err, "resolve first unknown bridge")
	require.True(ok)
	second, ok, err := resolver.resolve(
		ctx, "acct-opaque-8", "Mystery Bridge", "@bridge-b_bob:beeper.local")
	require.NoError(err, "resolve second unknown bridge")
	require.True(ok)

	assert.Equal("bridge-a", first.Slug)
	assert.Equal("bridge-b", second.Slug,
		"a cached opaque account must not hide a different observed bridge prefix")
}

func TestResolveConfiguredAccountServicePrecedesUserIDPrefix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	configured, created, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{
		Slug:                 "acct-custom",
		DisplayLabel:         "Configured Account Service",
		ScopePolicy:          store.ScopePolicyOptional,
		Normalization:        store.NormalizationLower,
		NormalizationVersion: 1,
	})
	require.NoError(err, "create configured account service")
	require.True(created)

	service, ok, err := newBridgeServiceResolver(st).resolve(
		ctx, "acct-custom", "Mystery Bridge", "@bridge-a_alice:beeper.local")
	require.NoError(err, "resolve configured account service")
	require.True(ok)
	assert.Equal(configured.ID, service.ID,
		"an explicit custom account mapping must remain authoritative")
}

func TestResolvePersistedOpaqueFallbackDoesNotHideUserIDPrefix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	fallback, ok, err := newBridgeServiceResolver(st).resolve(
		ctx, "acct-persisted", "", "@-invalid_alice:beeper.local")
	require.NoError(err, "register opaque account fallback")
	require.True(ok)
	assert.Equal("acct-persisted", fallback.Slug)

	service, ok, err := newBridgeServiceResolver(st).resolve(
		ctx, "acct-persisted", "Mystery Bridge", "@bridge-a_alice:beeper.local")
	require.NoError(err, "resolve qualified participant in a later run")
	require.True(ok)
	assert.Equal("bridge-a", service.Slug,
		"a persisted importer fallback must not hide a later explicit prefix")
}

func TestResolveInvalidBridgePrefixFallsBackToAccountID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	resolver := newBridgeServiceResolver(st)

	service, ok, err := resolver.resolve(
		context.Background(), "acct-fallback", "", "@-invalid_alice:beeper.local")
	require.NoError(err, "resolve bridge with invalid prefix")
	require.True(ok)
	assert.Equal("acct-fallback", service.Slug,
		"an invalid prefix must not block the safer account fallback")
}

func TestResolveUnclassifiableBridgeIsNotGuessed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	resolver := newBridgeServiceResolver(st)

	service, ok, err := resolver.resolve(context.Background(), "!!!", "", "alice")
	require.NoError(err, "resolve must not fail the import")
	assert.False(ok, "an unusable account id leaves the observation unclassified")
	assert.Nil(service)
}

func TestServiceScopeFollowsPolicyAndNeverFabricates(t *testing.T) {
	tests := []struct {
		name        string
		slugOrAlias string
		accountID   string
		userID      string
		wantKind    string
		wantValue   string
	}{
		{
			name:        "unscoped service records no scope",
			slugOrAlias: "whatsapp", accountID: "whatsapp",
			userID: "@15550100001:beeper.local",
		},
		{
			name:        "optional scope records the account",
			slugOrAlias: "google-chat", accountID: "googlechat-1",
			userID:    "@googlechat_alice:beeper.local",
			wantKind:  "account",
			wantValue: "googlechat-1",
		},
		{
			name:        "required scope records the account, not a claimed workspace",
			slugOrAlias: "slack", accountID: "slack-1",
			userID:    "@slack_alice:beeper.local",
			wantKind:  "account",
			wantValue: "slack-1",
		},
		{
			name:        "matrix records the observed server",
			slugOrAlias: "matrix", accountID: "matrix-1",
			userID:    "@alice:example.org",
			wantKind:  "server",
			wantValue: "example.org",
		},
		{
			name:        "matrix without a parseable server falls back to the account",
			slugOrAlias: "matrix", accountID: "matrix-1",
			userID:    "alice",
			wantKind:  "account",
			wantValue: "matrix-1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)

			service, err := st.ResolveCommunicationServiceContext(context.Background(), test.slugOrAlias)
			require.NoError(err, "resolve %s", test.slugOrAlias)

			kind, value := serviceScope(service, test.accountID, test.userID)
			if test.wantKind == "" {
				assert.Nil(kind, "scope kind")
				assert.Nil(value, "scope value")
				require.NoError(store.ValidateServiceScope(service, kind, value),
					"an unscoped service must reject a scope, so we must not send one")
				return
			}
			require.NotNil(kind, "scope kind")
			require.NotNil(value, "scope value")
			assert.Equal(test.wantKind, *kind)
			assert.Equal(test.wantValue, *value)
			require.NoError(store.ValidateServiceScope(service, kind, value),
				"the derived scope must satisfy the service's own policy")
		})
	}
}

func TestServiceScopeOfNilServiceIsEmpty(t *testing.T) {
	assert := assert.New(t)

	kind, value := serviceScope(nil, "acct-1", "@alice:example.org")
	assert.Nil(kind)
	assert.Nil(value)
}

func TestServiceSlugCandidate(t *testing.T) {
	tests := []struct{ raw, want string }{
		{raw: "WhatsApp", want: "whatsapp"},
		{raw: "Google Messages", want: "google-messages"},
		{raw: "google_chat", want: "google-chat"},
		{raw: "  Example  Bridge  ", want: "example-bridge"},
		{raw: "X", want: "x"},
		{raw: "bridge--v2", want: "bridge-v2"},
		{raw: "-leading-and-trailing-", want: "leading-and-trailing"},
		{raw: "!!!", want: ""},
		{raw: "", want: ""},
		{raw: "9lives", want: "9lives"},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			assert := assert.New(t)
			assert.Equal(test.want, serviceSlugCandidate(test.raw))
		})
	}
}
