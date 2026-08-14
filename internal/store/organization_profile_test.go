package store_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceOrganizationProfileRoundTripsEveryCollectionAndKeepsStableRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	uri := "https://example.com"
	first, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name: "Example Organisation", NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Identifiers: []store.OrganizationIdentifierInput{{
				IdentifierKind: store.OrganizationIdentifierKindDomain,
				Value:          "Example.COM", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Addresses: []store.OrganizationAddressInput{{
				AddressKind: store.PersonAddressPostal, StreetAddress: new("1 Example St"),
				Locality: new("Exampletown"), OriginalValue: "1 Example St;Exampletown",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			ContactPoints: []store.OrganizationContactPointInput{{
				AddressKind: store.ContactAddressEmail, OriginalValue: "INFO@EXAMPLE.COM",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Media: []store.OrganizationMediaInput{{
				MediaKind: store.PersonMediaLogo, URI: &uri, OriginalValue: uri,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Categories: []store.OrganizationCategoryInput{{
				Category: "Vendor", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	require.NoError(err)
	require.Len(first.Names, 1)
	require.Len(first.Identifiers, 1)
	require.Len(first.Addresses, 1)
	require.Len(first.ContactPoints, 1)
	require.Len(first.Media, 1)
	require.Len(first.Categories, 1)
	assert.Equal("example.com", first.Identifiers[0].NormalizedValue)
	assert.Equal("info@example.com", first.ContactPoints[0].NormalizedValue)
	assert.Equal(organization.Revision+1, first.Organization.Revision)

	second, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, first.Organization.Revision, store.OrganizationProfileInput{
			Names:         []store.OrganizationNameInput{{Name: "Example Organisation", NameKind: store.OrganizationNameKindAlias, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}},
			Identifiers:   []store.OrganizationIdentifierInput{{IdentifierKind: store.OrganizationIdentifierKindDomain, Value: "Example.COM", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}},
			Addresses:     []store.OrganizationAddressInput{{AddressKind: store.PersonAddressPostal, StreetAddress: new("1 Example St"), Locality: new("Exampletown"), OriginalValue: "1 Example St;Exampletown", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}},
			ContactPoints: []store.OrganizationContactPointInput{{AddressKind: store.ContactAddressEmail, OriginalValue: "INFO@EXAMPLE.COM", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}},
			Media:         []store.OrganizationMediaInput{{MediaKind: store.PersonMediaLogo, URI: &uri, OriginalValue: uri, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}},
		})
	require.NoError(err)
	assert.Equal(first.Names[0].Envelope.ID, second.Names[0].Envelope.ID)
	assert.Equal(first.Identifiers[0].Envelope.ID, second.Identifiers[0].Envelope.ID)
	assert.Equal(first.Addresses[0].Envelope.ID, second.Addresses[0].Envelope.ID)
	assert.Equal(first.ContactPoints[0].Envelope.ID, second.ContactPoints[0].Envelope.ID)
	assert.Equal(first.Media[0].Envelope.ID, second.Media[0].Envelope.ID)
	assert.Empty(second.Categories)

	history, err := st.GetOrganizationProfileContext(ctx, organization.ID, true)
	require.NoError(err)
	require.Len(history.Categories, 1)
	assert.False(history.Categories[0].Envelope.IsCurrent())
}

func TestReplaceOrganizationProfileAppendsOmittedOrdinalsAcrossHistory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(
		ctx, store.OrganizationInput{Name: "Example Org"})
	requirements.NoError(err)

	first, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Categories: []store.OrganizationCategoryInput{
				{Category: "Vendor", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
				{Category: "Partner", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
			},
		})
	requirements.NoError(err)
	requirements.Len(first.Categories, 2)
	assertions.Equal(0, first.Categories[0].Envelope.Ordinal)
	assertions.Equal(1, first.Categories[1].Envelope.Ordinal)

	second, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, first.Organization.Revision, store.OrganizationProfileInput{
			Categories: []store.OrganizationCategoryInput{{
				Category: "Customer", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	requirements.NoError(err)
	requirements.Len(second.Categories, 1)
	assertions.Equal(2, second.Categories[0].Envelope.Ordinal,
		"automatic ordinals must not reuse a superseded history slot")
}

func TestReplaceOrganizationProfileValidatesBeforeWriting(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(
		ctx, store.OrganizationInput{Name: "Example Org"})
	require.NoError(err)

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision+1, store.OrganizationProfileInput{})
	require.ErrorIs(err, store.ErrOrganizationRevisionConflict)

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name: " ", NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "names[0].name is required")

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{
				{Name: "Example Org", NameKind: store.OrganizationNameKindAlias, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
				{Name: " example   org ", NameKind: store.OrganizationNameKindAlias, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
			},
		})
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "names[1] duplicates names[0]")

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Categories: []store.OrganizationCategoryInput{{
				Category: "Vendor", Envelope: store.ValueEnvelopeInput{Source: "guessed"},
			}},
		})
	require.ErrorIs(err, store.ErrInvalidProvenance)
}

func TestReplaceOrganizationProfileRejectsProviderIdentityContactPoint(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(
		ctx, store.OrganizationInput{Name: "Example Org"})
	require.NoError(err)

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			ContactPoints: []store.OrganizationContactPointInput{{
				AddressKind:   store.ContactAddressProviderIdentity,
				OriginalValue: "provider:opaque-identity",
				Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "address_kind \"provider_identity\" is not exportable")

	profile, err := st.GetOrganizationProfileContext(ctx, organization.ID, false)
	require.NoError(err)
	require.Empty(profile.ContactPoints)
	require.Equal(organization.Revision, profile.Organization.Revision)
}

func TestRemovingFutureOrganizationProfileRowRetractsWithoutInvalidWorldInterval(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(
		ctx, store.OrganizationInput{Name: "Example Org"})
	require.NoError(err)
	future := time.Now().UTC().Add(24 * time.Hour)

	withFuture, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name: "Future Alias", NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceUser, ActiveFrom: &future,
				},
			}},
		})
	require.NoError(err)

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, withFuture.Organization.Revision,
		store.OrganizationProfileInput{})
	require.NoError(err)
	history, err := st.GetOrganizationProfileContext(ctx, organization.ID, true)
	require.NoError(err)
	require.Len(history.Names, 1)
	assert.Nil(history.Names[0].Envelope.ActiveUntil)
	assert.NotNil(history.Names[0].Envelope.SupersededAt)
	require.NotNil(history.Names[0].Envelope.ActiveFrom)
	assert.WithinDuration(future, *history.Names[0].Envelope.ActiveFrom, time.Microsecond)
}

func TestGetOrganizationProfileRejectsMissingOrganization(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrganizationProfileContext(context.Background(), 9999, false)
	require.ErrorIs(err, store.ErrOrganizationNotFound)
}

func TestGetOrganizationProfileReturnsOneRootAndChildrenSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(
		ctx, store.OrganizationInput{Name: "Old Root"})
	require.NoError(err)
	initial, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name: "Old Alias", NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	require.NoError(err)
	require.Len(initial.Names, 1)

	writer, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = writer.Rollback()
		}
	})
	_, err = writer.ExecContext(ctx, st.Rebind(`
		UPDATE organizations
		SET name = ?, name_normalized = ?, revision = revision + 1
		WHERE id = ?
	`), "New Root", "new root", organization.ID)
	require.NoError(err)
	_, err = writer.ExecContext(ctx, st.Rebind(`
		UPDATE organization_names
		SET formatted = ?, original_value = ?, name_normalized = ?
		WHERE id = ?
	`), "New Alias", "New Alias", "new alias", initial.Names[0].Envelope.ID)
	require.NoError(err)

	rootRead := make(chan struct{})
	releaseRootRead := make(chan struct{})
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(&organizationProfileBarrierHandler{
		rootRead: rootRead, release: releaseRootRead,
	}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	type result struct {
		profile *store.OrganizationProfile
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		profile, readErr := st.GetOrganizationProfileContext(ctx, organization.ID, false)
		resultCh <- result{profile: profile, err: readErr}
	}()

	select {
	case <-rootRead:
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(writer.Commit())
	committed = true
	close(releaseRootRead)

	got := <-resultCh
	require.NoError(got.err)
	require.Len(got.profile.Names, 1)
	switch got.profile.Organization.Name {
	case "Old Root":
		assert.Equal(initial.Organization.Revision, got.profile.Organization.Revision)
		assert.Equal("Old Alias", got.profile.Names[0].Name)
	case "New Root":
		assert.Equal(initial.Organization.Revision+1, got.profile.Organization.Revision)
		assert.Equal("New Alias", got.profile.Names[0].Name)
	default:
		assert.Fail("unexpected root snapshot", got.profile.Organization.Name)
	}
}

type organizationProfileBarrierHandler struct {
	rootRead chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (h *organizationProfileBarrierHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *organizationProfileBarrierHandler) Handle(_ context.Context, record slog.Record) error {
	var kind, statement string
	record.Attrs(func(attribute slog.Attr) bool {
		switch attribute.Key {
		case "kind":
			kind = attribute.Value.String()
		case "stmt":
			statement = attribute.Value.String()
		}
		return true
	})
	if kind == "queryrow" &&
		strings.Contains(statement, "FROM organizations WHERE id =") {
		h.once.Do(func() {
			close(h.rootRead)
			<-h.release
		})
	}
	return nil
}

func (h *organizationProfileBarrierHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *organizationProfileBarrierHandler) WithGroup(string) slog.Handler {
	return h
}

var _ slog.Handler = (*organizationProfileBarrierHandler)(nil)

func TestOrganizationProfileKeepsValueDuplicatesWithDistinctIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Duplicate Value Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	identity := func(propID string) store.ValueEnvelopeInput {
		return store.ValueEnvelopeInput{
			Source: store.ProvenanceVCardImport, SourceRef: new("vcard:duplicate-org"),
			VCard: store.VCardIdentity{Property: "ADR", PropID: &propID},
		}
	}
	input := store.OrganizationProfileInput{
		Addresses: []store.OrganizationAddressInput{
			{
				AddressKind: store.PersonAddressPostal, StreetAddress: new("1 Example St"),
				OriginalValue: "1 Example St", Envelope: identity("adr-1"),
			},
			{
				AddressKind: store.PersonAddressPostal, StreetAddress: new("1 Example St"),
				OriginalValue: "1 Example St", Envelope: identity("adr-2"),
			},
		},
		Media: []store.OrganizationMediaInput{
			{
				MediaKind: store.PersonMediaLogo, Data: []byte("shared-logo-bytes"),
				Envelope: store.ValueEnvelopeInput{Ordinal: new(0), Source: store.ProvenanceUser},
			},
			{
				MediaKind: store.PersonMediaLogo, Data: []byte("shared-logo-bytes"),
				Envelope: store.ValueEnvelopeInput{Ordinal: new(1), Source: store.ProvenanceUser},
			},
		},
	}
	first, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, input)
	require.NoError(err,
		"rows sharing a value under distinct PROP-IDs or ordinals are not duplicates")
	require.Len(first.Addresses, 2)
	require.Len(first.Media, 2)

	firstIDs := []int64{first.Addresses[0].Envelope.ID, first.Addresses[1].Envelope.ID,
		first.Media[0].Envelope.ID, first.Media[1].Envelope.ID}
	second, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, first.Organization.Revision, input)
	require.NoError(err)
	require.Len(second.Addresses, 2)
	require.Len(second.Media, 2)
	secondIDs := []int64{second.Addresses[0].Envelope.ID, second.Addresses[1].Envelope.ID,
		second.Media[0].Envelope.ID, second.Media[1].Envelope.ID}
	assert.ElementsMatch(firstIDs, secondIDs,
		"an identical replacement must retain every row rather than rewriting them")
}
