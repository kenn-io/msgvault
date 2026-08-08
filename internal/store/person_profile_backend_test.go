package store_test

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

var profileEnvelopeColumnNames = []string{
	"pref", "ordinal", "type_label", "type_tokens",
	"vcard_property", "vcard_group", "vcard_prop_id", "vcard_pid", "vcard_altid",
	"source", "source_ref", "confidence",
	"active_from", "active_until",
	"created_at", "updated_at", "superseded_at",
}

func TestProfileTableColumnsMatchOnTheConfiguredBackend(t *testing.T) {
	assert := assert.New(t)
	st := storetest.New(t).Store

	valueTableColumns := func(specific ...string) []string {
		columns := append([]string{"id", "person_id"}, specific...)
		return append(columns, profileEnvelopeColumnNames...)
	}

	tests := []struct {
		table string
		want  []string
	}{
		{
			table: "communication_services",
			want: []string{
				"id", "slug", "display_label", "scope_policy", "default_scope_kind",
				"normalization", "normalization_version", "uri_scheme",
				"profile_url_template", "is_system", "is_active",
				"created_at", "updated_at",
			},
		},
		{table: "communication_service_aliases", want: []string{"alias", "service_id"}},
		{
			table: "person_names",
			want: valueTableColumns(
				"name_kind", "formatted", "family_name", "given_name",
				"additional_names", "honorific_prefixes", "honorific_suffixes",
				"secondary_surname", "generation", "language", "script",
				"phonetic_system", "phonetic_script", "sort_as", "is_derived",
				"original_value",
			),
		},
		{
			table: "person_contact_points",
			want: valueTableColumns(
				"address_kind", "service_id", "scope_kind", "scope_value",
				"original_value", "normalized_value", "normalization",
				"normalization_version", "uri",
			),
		},
		{
			table: "person_addresses",
			want: valueTableColumns(
				"address_kind", "post_office_box", "extended_address",
				"street_address", "locality", "region", "postal_code",
				"country_name", "extended_components", "free_text", "label",
				"geo_uri", "timezone", "country_code", "place_uri",
				"original_value",
			),
		},
		{
			table: "person_dates",
			want: valueTableColumns(
				"date_kind", "label", "date_year", "date_month", "date_day",
				"date_text", "calendar_scale", "original_value",
			),
		},
		{
			table: "person_categories",
			want:  valueTableColumns("original_value", "normalized_value"),
		},
		{
			table: "person_media",
			want: valueTableColumns(
				"media_kind", "media_type", "uri", "data", "byte_size",
				"content_hash", "original_value",
			),
		},
		{
			table: "participant_contact_observations",
			want: append(
				[]string{
					"id", "participant_id", "source_id", "address_kind",
					"service_id", "scope_kind", "scope_value", "provider_user_id",
					"original_value", "normalized_value", "normalization",
					"normalization_version", "observed_at",
				},
				profileEnvelopeColumnNames...,
			),
		},
		{
			table: "identity_match_candidates",
			want: []string{
				"id", "left_kind", "left_id", "right_kind", "right_id", "basis",
				"service_id", "scope_kind", "scope_value", "normalized_value",
				"state", "confidence", "source", "source_ref",
				"decided_by", "decided_at", "notes", "created_at", "updated_at",
			},
		},
		{
			table: "identity_match_evidence",
			want: []string{
				"id", "candidate_id", "evidence_kind", "evidence_ref", "detail",
				"source", "created_at",
			},
		},
	}
	for _, tc := range tests {
		got := liveTableColumns(t, st, tc.table)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		assert.Equal(want, got, "column set for %s", tc.table)
	}
}

func TestParticipantIdentifiersGainedServiceScopeColumns(t *testing.T) {
	assert := assert.New(t)
	st := storetest.New(t).Store

	columns := liveTableColumns(t, st, "participant_identifiers")
	for _, column := range []string{"service_id", "scope_kind", "scope_value"} {
		assert.Contains(columns, column)
	}
	for _, column := range []string{"identifier_type", "identifier_value", "participant_id"} {
		assert.Contains(columns, column, "the existing anchor columns must be untouched")
	}
}

func TestProfileReadsSucceedOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	services, err := st.ListCommunicationServicesContext(ctx, true)
	require.NoError(err, "communication_services and its alias table")
	assert.GreaterOrEqual(len(services), 24)

	personID := newTestPerson(t, st)
	profile, err := st.GetPersonProfileContext(ctx, personID)
	require.NoError(err, "person profile value tables")
	assert.Empty(profile.Names)

	history, err := st.GetPersonProfileHistoryContext(ctx, personID)
	require.NoError(err, "participant_contact_observations")
	assert.Empty(history.Observations)

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err, "identity match candidates and evidence")
	assert.Empty(candidates)
}

func TestFullProfileLifecycleOnConfiguredBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "alice@example.com", "Alice Example",
	)
	require.NoError(err, "EnsureParticipantByIdentifier")
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
	require.NoError(err, "CreatePersonFromParticipantContext")
	seedFullProfile(t, st, person.ID)

	profile, err := st.GetPersonProfileContext(ctx, person.ID)
	require.NoError(err, "GetPersonProfileContext")
	require.Len(profile.Names, 2)
	require.Len(profile.ContactPoints, 2)
	require.Len(profile.Media, 1)

	data, mediaType, err := st.ReadPersonMediaDataContext(
		ctx, person.ID, profile.Media[0].Envelope.ID,
	)
	require.NoError(err, "ReadPersonMediaDataContext")
	assert.Equal([]byte("synthetic-photo"), data)
	assert.Equal("image/png", mediaType)

	patched, err := st.ApplyPersonProfilePatchContext(
		ctx, person.ID, profile.Person.Revision, store.PersonProfilePatch{
			ContactPoints: &store.PersonContactPointPatch{
				Supersede: []int64{profile.ContactPoints[0].Envelope.ID},
				Add: []store.PersonContactPointInput{{
					AddressKind:   store.ContactAddressUsername,
					ServiceSlug:   new("matrix"),
					ScopeKind:     new("server"),
					ScopeValue:    new("example.org"),
					OriginalValue: "@Alice:example.org",
					Envelope:      store.ValueEnvelope{Source: store.ProvenanceUser},
				}},
			},
		},
	)
	require.NoError(err, "ApplyPersonProfilePatchContext")
	assert.Equal(profile.Person.Revision+1, patched.Person.Revision)
	require.Len(patched.ContactPoints, 2)
	assert.Equal("@Alice:example.org", patched.ContactPoints[1].NormalizedValue)

	bobID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@bob:example.org", "Bob Example",
	)
	require.NoError(err, "EnsureParticipantByIdentifier bob")
	conflicting, err := st.RecordContactObservationContext(
		ctx, bobID, store.ParticipantContactObservationInput{
			AddressKind:    store.ContactAddressUsername,
			ServiceSlug:    new("x"),
			ProviderUserID: new("x-bob"),
			OriginalValue:  "@shared",
			Envelope:       store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
		},
	)
	require.NoError(err, "first observation")
	assert.False(conflicting.Conflicting)

	second, err := st.RecordContactObservationContext(
		ctx, participantID, store.ParticipantContactObservationInput{
			AddressKind:    store.ContactAddressUsername,
			ServiceSlug:    new("x"),
			ProviderUserID: new("x-alice"),
			OriginalValue:  "@shared",
			Envelope:       store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
		},
	)
	require.NoError(err, "second observation")
	assert.True(second.Conflicting)

	history, err := st.GetPersonProfileHistoryContext(ctx, person.ID)
	require.NoError(err, "GetPersonProfileHistoryContext")
	assert.Len(history.ContactPoints, 3)
	assert.Len(history.Observations, 1)
}

func liveTableColumns(t *testing.T, st *store.Store, table string) []string {
	t.Helper()
	require := require.New(t)

	rows, err := st.DB().QueryContext(
		context.Background(), "SELECT * FROM "+table+" WHERE 1 = 0",
	)
	require.NoError(err, "table %s must exist and be queryable", table)
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	require.NoError(err, "read columns of %s", table)
	require.NoError(rows.Err(), "iterate columns of %s", table)
	sort.Strings(columns)
	return columns
}
