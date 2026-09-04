package store_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

var profileEnvelopeColumnNames = []string{
	"pref", "ordinal", "type_label", "type_tokens",
	"vcard_property", "vcard_group", "vcard_prop_id", "vcard_pid", "vcard_altid",
	"source", "source_ref", "source_resource_uid", "confidence",
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
				"state", "confidence", "source", "source_ref", "observation_conflict_origin",
				"pre_conflict_state", "application_pending",
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

func TestInitSchemaAddsObservationConflictOriginToExistingCandidateTable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()

	for _, column := range []string{
		"observation_conflict_origin", "pre_conflict_state", "application_pending",
	} {
		if column == "application_pending" {
			_, err := st.DB().ExecContext(ctx,
				"DROP INDEX idx_identity_match_candidates_application_pending",
			)
			require.NoError(err)
		}
		_, err := st.DB().ExecContext(
			ctx, "ALTER TABLE identity_match_candidates DROP COLUMN "+column,
		)
		require.NoError(err)
	}
	require.NoError(st.InitSchemaContext(ctx))

	columns := liveTableColumns(t, st, "identity_match_candidates")
	assert.Contains(columns, "observation_conflict_origin")
	assert.Contains(columns, "pre_conflict_state")
	assert.Contains(columns, "application_pending")
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

func TestDirectoryPeoplePageSucceedsOnTheConfiguredBackend(t *testing.T) {
	st := storetest.New(t).Store
	alice := createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")
	createDirectoryPerson(t, st, "Alice Other", "other@example.test", "colleague", "active", "Other Co")

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
		Query: "alcie", Category: "friend", Organization: "acme", Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	assert.Equal(t, alice.ID, page.People[0].ID)
}

// This uses storetest's selected backend (SQLite by default, PostgreSQL when
// MSGVAULT_TEST_DB is configured) to keep keyset ordering identical across
// the exact, prefix, and one-edit tiers.
func TestDirectoryPeoplePageSequenceOnTheConfiguredBackend(t *testing.T) {
	st := storetest.New(t).Store
	exact := createDirectoryPerson(t, st, "Alice Exact", "alice-exact@example.test", "friend", "active", "Acme")
	prefix := createDirectoryPerson(t, st, "Alicef Prefix", "alicef-prefix@example.test", "friend", "active", "Acme")
	fuzzy := createDirectoryPerson(t, st, "Alicf Fuzzy", "alicf-fuzzy@example.test", "friend", "active", "Acme")

	var got []int64
	cursor := ""
	for pageNumber := 0; ; pageNumber++ {
		page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
			Query: "alice", Limit: 1, Cursor: cursor,
		})
		require.NoError(t, err)
		got = append(got, directoryPersonIDs(page.People)...)
		if page.NextCursor == "" {
			break
		}
		require.Less(t, pageNumber, 3, "directory cursor must make bounded progress")
		cursor = page.NextCursor
	}

	assert.Equal(t, []int64{exact.ID, prefix.ID, fuzzy.ID}, got)
}

// This runs on the configured backend and protects the persisted canonical
// order key from whitespace or Unicode collation drift between page requests.
func TestDirectoryPeopleUnicodeCursorSequenceOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	first := createDirectoryPerson(t, st, "Ålice  a", "unicode-first@sample.test", "friend", "active", "Acme")
	second := createDirectoryPerson(t, st, "Ålice z", "unicode-second@sample.test", "friend", "active", "Acme")

	pageOne, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.NotEmpty(pageOne.NextCursor)
	pageTwo, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: pageOne.NextCursor})
	require.NoError(err)
	assert.Equal(t, []int64{first.ID, second.ID}, append(directoryPersonIDs(pageOne.People), directoryPersonIDs(pageTwo.People)...))
}

func TestDirectoryPeopleLastContactRangeAndCursorOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	oldest := createDirectoryPerson(t, st, "Alice Oldest", "last-contact-oldest@sample.test", "friend", "inactive", "Acme")
	middle := createDirectoryPerson(t, st, "Bob Middle", "last-contact-middle@sample.test", "friend", "inactive", "Acme")
	newest := createDirectoryPerson(t, st, "Carol Newest", "last-contact-newest@sample.test", "friend", "inactive", "Acme")

	oldestAt := time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC)
	middleAt := oldestAt.Add(24 * time.Hour)
	newestAt := middleAt.Add(500 * time.Millisecond)
	for _, contact := range []struct {
		personID int64
		at       time.Time
	}{{oldest.ID, oldestAt}, {middle.ID, middleAt}, {newest.ID, newestAt}} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_contact_state (
			person_id, last_contact_at, interaction_count
		) VALUES (?, ?, 1)`), contact.personID, contact.at)
		require.NoError(err)
	}

	query := store.DirectoryPeopleQuery{
		LastContactAfter:  &middleAt,
		LastContactBefore: &newestAt,
		Sort:              store.DirectoryPeopleSortLastContactDesc,
		Limit:             1,
	}
	first, err := st.DirectoryPeoplePageContext(t.Context(), query)
	require.NoError(err)
	require.Len(first.People, 1)
	assert.Equal(newest.ID, first.People[0].ID)
	require.NotNil(first.People[0].LastContactAt)
	assert.Equal(newestAt, *first.People[0].LastContactAt)
	require.NotEmpty(first.NextCursor)

	query.Cursor = first.NextCursor
	second, err := st.DirectoryPeoplePageContext(t.Context(), query)
	require.NoError(err)
	assert.Equal([]int64{middle.ID}, directoryPersonIDs(second.People))
	assert.Empty(second.NextCursor)

	query.Sort = store.DirectoryPeopleSortLastContactAsc
	_, err = st.DirectoryPeoplePageContext(t.Context(), query)
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)

	exact, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
		LastContactAfter: &middleAt, LastContactBefore: &middleAt,
	})
	require.NoError(err)
	assert.Equal([]int64{middle.ID}, directoryPersonIDs(exact.People))
}

// Delete keys are only an indexed prefilter: this configured-backend fixture
// proves the actual canonical token distance before Directory returns a row.
func TestDirectoryPeopleFuzzyTokenDistanceOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	insert := createDirectoryPerson(t, st, "abc", "insert@sample.test", "friend", "active", "Acme")
	deleted := createDirectoryPerson(t, st, "abcde", "delete@sample.test", "friend", "active", "Acme")
	substitute := createDirectoryPerson(t, st, "abxd", "substitute@sample.test", "friend", "active", "Acme")
	transpose := createDirectoryPerson(t, st, "acbd", "transpose@sample.test", "friend", "active", "Acme")
	invalid := createDirectoryPerson(t, st, "abcx", "invalid@sample.test", "friend", "active", "Acme")
	invalidTranspose := createDirectoryPerson(t, st, "abac", "invalid-transpose@sample.test", "friend", "active", "Acme")

	for _, tc := range []struct {
		query string
		want  int64
	}{{"abcd", insert.ID}, {"abcd", deleted.ID}, {"abcd", substitute.ID}, {"abcd", transpose.ID}} {
		page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: tc.query})
		require.NoError(err)
		assert.Contains(directoryPersonIDs(page.People), tc.want)
	}
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "axbc"})
	require.NoError(err)
	assert.NotContains(directoryPersonIDs(page.People), invalid.ID)
	page, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "baca"})
	require.NoError(err)
	assert.NotContains(directoryPersonIDs(page.People), invalidTranspose.ID)
}

// False delete-key collisions must not consume a complete public page before
// a later verified one-edit match. The resumed page starts after the exact
// prior row and must still scan past those false raw candidates.
func TestDirectoryPeopleFuzzyCollisionPagingOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	exact := createDirectoryPerson(t, st, "axbc", "collision-exact@sample.test", "friend", "active", "Acme")
	for index := range 65 {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?)`), fmt.Sprintf("collision-false-%03d", index), "abcx")
		require.NoError(err)
	}
	verified := createDirectoryPerson(t, st, "axbd", "collision-verified@sample.test", "friend", "active", "Acme")

	first, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "axbc", Limit: 1})
	require.NoError(err)
	assert.Equal([]int64{exact.ID}, directoryPersonIDs(first.People))
	require.NotEmpty(first.NextCursor)

	second, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "axbc", Limit: 1, Cursor: first.NextCursor})
	require.NoError(err)
	assert.Equal([]int64{verified.ID}, directoryPersonIDs(second.People))
	assert.Empty(second.NextCursor)
}

// Moving a current employment through a raw update must queue both the old
// and new people, so refresh never leaves the old Directory projection stale.
func TestDirectoryProjectionEmploymentMoveOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	first := createDirectoryPerson(t, st, "First Person", "move-first@sample.test", "friend", "active", "Shared Org")
	second := createDirectoryPerson(t, st, "Second Person", "move-second@sample.test", "friend", "active", "Other Org")
	employments, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{PersonID: first.ID, CurrentOnly: true})
	require.NoError(err)
	require.Len(employments, 1)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE employments SET is_primary = FALSE WHERE person_id = ?`), second.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE employments SET person_id = ? WHERE id = ?`), second.ID, employments[0].ID)
	require.NoError(err)
	require.NoError(st.RefreshDirectoryProjectionContext(t.Context()))

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Organization: "shared org"})
	require.NoError(err)
	assert.Equal(t, []int64{second.ID}, directoryPersonIDs(page.People))
}

// Two writers that refresh the same dirty person at the same time must both
// succeed. PostgreSQL READ COMMITTED lets both transactions read the same
// dirty row, so the second refresh must tolerate the first one committing
// its projection rows in between.
func TestDirectoryProjectionConcurrentRefreshOnTheConfiguredBackend(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	person := createDirectoryPerson(t, st, "Shared Person", "shared@sample.test", "friend", "active", "Shared Org")
	ctx := t.Context()

	const workers, rounds = 2, 40
	start := make(chan struct{})
	results := make(chan error, workers)
	for worker := range workers {
		go func() {
			<-start
			for round := range rounds {
				name := fmt.Sprintf("Shared Person %d-%d", worker, round)
				if _, err := st.DB().ExecContext(ctx, st.Rebind(`UPDATE persons SET display_name = ? WHERE id = ?`), name, person.ID); err != nil {
					results <- fmt.Errorf("dirty person: %w", err)
					return
				}
				if err := st.RefreshDirectoryProjectionContext(ctx); err != nil {
					results <- fmt.Errorf("refresh round %d: %w", round, err)
					return
				}
			}
			results <- nil
		}()
	}
	close(start)
	for range workers {
		select {
		case err := <-results:
			require.NoError(err)
		case <-time.After(60 * time.Second):
			require.FailNow("concurrent Directory refresh did not finish")
		}
	}

	require.NoError(st.RefreshDirectoryProjectionContext(ctx))
	var projected, dirty int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM directory_people WHERE person_id = ?`), person.ID).Scan(&projected))
	require.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM directory_projection_dirty`).Scan(&dirty))
	assert.Equal(t, 1, projected)
	assert.Zero(t, dirty)
	page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Query: "shared"})
	require.NoError(err)
	assert.Equal(t, []int64{person.ID}, directoryPersonIDs(page.People))
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
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
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
			Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
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
			Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
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
