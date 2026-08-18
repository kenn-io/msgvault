package beeper

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// newBeeperTestSource creates a synthetic Beeper source for the given
// accountID, mirroring what add-beeper does.
func newBeeperTestSource(t *testing.T, st *store.Store, accountID string) int64 {
	t.Helper()
	require := require.New(t)

	source, err := st.GetOrCreateSource(sourceTypeBeeper, accountID)
	require.NoError(err, "GetOrCreateSource")
	return source.ID
}

// participantForBeeperUser returns the participant that owns a Beeper user ID
// anchor, going through the same participant_identifiers lookup the ladder
// writes.
func participantForBeeperUser(t *testing.T, st *store.Store, userID string) int64 {
	t.Helper()
	require := require.New(t)

	var participantID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT participant_id FROM participant_identifiers
		 WHERE identifier_type = ?
		   AND (identifier_value = ? OR identifier_value LIKE ?)`),
		participantIdentifierType, userID, "%"+userID,
	).Scan(&participantID), "lookup participant for %s", userID)
	return participantID
}

// observationValues maps address kind to normalized value for a participant's
// current observations, so assertions stay readable.
func observationValues(
	t *testing.T, st *store.Store, participantID int64,
) map[store.ContactAddressKind]string {
	t.Helper()
	require := require.New(t)

	observations, err := st.ListParticipantObservationsContext(
		context.Background(), participantID, true)
	require.NoError(err, "ListParticipantObservationsContext")
	values := make(map[store.ContactAddressKind]string, len(observations))
	for _, observation := range observations {
		values[observation.AddressKind] = observation.NormalizedValue
	}
	return values
}

// observationChat builds a one-message chat whose participant list carries the
// addresses under test. The timestamp sits well outside reconcileWindow so
// the head re-walk terminates immediately.
func observationChat(
	accountID, network, senderID string, participants ...map[string]any,
) *fakeChat {
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	chat := &fakeChat{
		ID:        "!obs-" + accountID + ":beeper.local",
		AccountID: accountID, Network: network,
		Title: "Observations", Type: "group",
		Participants: participants,
		Msgs: []fakeMsg{{
			ID: "m1", SortKey: 1, Timestamp: base, Text: "hello", SenderID: senderID,
		}},
	}
	chat.LastActivity = base
	return chat
}

func TestImportCapturesEveryObservedAddressForOneParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{
			"id":          "@telegram_alice:beeper.local",
			"fullName":    "Alice Example",
			"username":    "@Alice",
			"phoneNumber": "+12025550123",
			"email":       "Alice@Example.com",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	summary, err := imp.Import(context.Background(), ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "Import")
	assert.Positive(summary.ObservationsRecorded, "capture must report what it recorded")

	participantID := participantForBeeperUser(t, st, "@telegram_alice:beeper.local")
	observations, err := st.ListParticipantObservationsContext(context.Background(), participantID, true)
	require.NoError(err, "ListParticipantObservationsContext")
	require.Len(observations, 3, "phone, email, and username all attach to one participant")

	values := observationValues(t, st, participantID)
	assert.Equal("+12025550123", values[store.ContactAddressPhone])
	assert.Equal("alice@example.com", values[store.ContactAddressEmail])
	assert.Equal("alice", values[store.ContactAddressUsername],
		"telegram's strip_at_lower strategy removes the leading @ and lowercases")

	for _, observation := range observations {
		require.NotNil(observation.ServiceSlug, "%s must be service classified", observation.AddressKind)
		assert.Equal("telegram", *observation.ServiceSlug)
		require.NotNil(observation.ProviderUserID, "%s must carry a stable anchor", observation.AddressKind)
		assert.Equal("beeper:8:telegram:28:@telegram_alice:beeper.local", *observation.ProviderUserID)
		assert.Equal(store.ProvenanceArchiveObservation, observation.Envelope.Source)
		require.NotNil(observation.Envelope.SourceRef)
		assert.Equal("beeper:telegram:@telegram_alice:beeper.local", *observation.Envelope.SourceRef)
		assert.Nil(observation.Envelope.Confidence,
			"an observation is something we saw, not a probability")
		require.NotNil(observation.ObservedAt)
	}
	assert.Equal("Alice@Example.com", observationOriginal(t, st, participantID, store.ContactAddressEmail),
		"the source rendering is always retained")
}

// observationOriginal returns the original (unnormalized) value of one current
// observation of the given kind.
func observationOriginal(
	t *testing.T, st *store.Store, participantID int64, kind store.ContactAddressKind,
) string {
	t.Helper()
	require := require.New(t)

	observations, err := st.ListParticipantObservationsContext(context.Background(), participantID, true)
	require.NoError(err, "ListParticipantObservationsContext")
	for _, observation := range observations {
		if observation.AddressKind == kind {
			return observation.OriginalValue
		}
	}
	require.Failf("no observation", "no current %s observation for participant %d", kind, participantID)
	return ""
}

func TestImportRecordsAUsernameThePhoneServiceRejectsWithoutAService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// whatsapp is seeded with normalization phone_e164, which applies to every
	// address kind, so a non-phone username cannot be normalized for it. The
	// value must still be captured, just unclassified.
	f := newFakeBeeper(t)
	f.addChat(observationChat("whatsapp", "WhatsApp", "@15550100001:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{
			"id":          "@15550100001:beeper.local",
			"fullName":    "Alice Example",
			"username":    "alice.example",
			"phoneNumber": "+1 (202) 555-0123",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "whatsapp", NoMedia: true})
	require.NoError(err, "Import")

	participantID := participantForBeeperUser(t, st, "@15550100001:beeper.local")
	observations, err := st.ListParticipantObservationsContext(context.Background(), participantID, true)
	require.NoError(err, "ListParticipantObservationsContext")
	require.Len(observations, 2)

	byKind := map[store.ContactAddressKind]store.ParticipantContactObservation{}
	for _, observation := range observations {
		byKind[observation.AddressKind] = observation
	}
	phone := byKind[store.ContactAddressPhone]
	require.NotNil(phone.ServiceSlug)
	assert.Equal("whatsapp", *phone.ServiceSlug)
	assert.Equal("+12025550123", phone.NormalizedValue)

	username := byKind[store.ContactAddressUsername]
	assert.Nil(username.ServiceSlug,
		"a value the service normalization rejects is recorded unclassified, not dropped")
	assert.Equal("alice.example", username.NormalizedValue)
	assert.Equal("alice.example", username.OriginalValue)
}

func TestProviderIdentityEvidencePreservesExactValueForPhoneService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	sourceID := newBeeperTestSource(t, st, "whatsapp")
	firstID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@whatsapp_case_one:beeper.local", "Case One")
	require.NoError(err, "ensure first participant")
	secondID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@whatsapp_case_two:beeper.local", "Case Two")
	require.NoError(err, "ensure second participant")
	cc := captureContext{SourceID: sourceID, AccountID: "whatsapp", Network: "WhatsApp"}

	_, err = recorder.capture(ctx, firstID, &User{
		ID:  "@whatsapp_case_one:beeper.local",
		Raw: []byte(`{"id":"@whatsapp_case_one:beeper.local","providerID":"NativeCaseID"}`),
	}, cc)
	require.NoError(err, "capture first provider identity")
	_, err = newObservationRecorder(st).capture(ctx, secondID, &User{
		ID:  "@whatsapp_case_two:beeper.local",
		Raw: []byte(`{"id":"@whatsapp_case_two:beeper.local","providerID":"nativecaseid"}`),
	}, cc)
	require.NoError(err, "capture second provider identity")

	first, err := st.ListParticipantObservationsContext(ctx, firstID, true)
	require.NoError(err, "list first observations")
	second, err := st.ListParticipantObservationsContext(ctx, secondID, true)
	require.NoError(err, "list second observations")
	require.Len(first, 1)
	require.Len(second, 1)
	assert.Equal(store.ContactAddressProviderIdentity, first[0].AddressKind)
	assert.Equal(store.ContactAddressProviderIdentity, second[0].AddressKind)
	assert.Equal(first[0].NormalizedValue, first[0].OriginalValue)
	assert.Equal(second[0].NormalizedValue, second[0].OriginalValue)
	assert.NotEqual(first[0].NormalizedValue, second[0].NormalizedValue,
		"provider identity evidence is case-sensitive")
	assert.Equal("none", first[0].Normalization,
		"provider identity evidence must not inherit phone normalization")
	assert.Equal("none", second[0].Normalization)
	require.NotNil(first[0].ServiceSlug)
	require.NotNil(second[0].ServiceSlug)
	assert.Equal("whatsapp", *first[0].ServiceSlug)
	assert.Equal("whatsapp", *second[0].ServiceSlug)
}

func TestObservationDedupKeepsChangedProviderIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	recorder := newObservationRecorder(st)
	sourceID := newBeeperTestSource(t, st, "telegram")
	participantID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@provider-change:beeper.local", "Provider Change")
	require.NoError(err)
	cc := captureContext{
		SourceID: sourceID, AccountID: "telegram", Network: "Telegram",
	}
	user := &User{
		ID: "@provider-change:beeper.local", Username: "same-user",
	}

	first, err := recorder.capture(ctx, participantID, user, cc)
	require.NoError(err)
	require.Len(first, 1)
	require.NotNil(first[0].Observation.ProviderUserID)
	fallbackID := *first[0].Observation.ProviderUserID

	user.Raw = []byte(`{"id":"@provider-change:beeper.local","providerID":"native-user-id"}`)
	second, err := recorder.capture(ctx, participantID, user, cc)
	require.NoError(err)
	require.Len(second, 1,
		"a changed provider identity must bypass run-local observation deduplication")
	require.NotNil(second[0].Observation.ProviderUserID)
	assert.NotEqual(fallbackID, *second[0].Observation.ProviderUserID)
	assert.Equal(
		providerIdentityKey("telegram", "account", "telegram", "native-user-id"),
		*second[0].Observation.ProviderUserID,
	)
}

func TestImportUsesRequestedAccountForObservationIdentityScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	sharedProvider := "same-native-id"
	first := observationChat("hostile-account", "Telegram",
		"@telegram_account_a:beeper.local", map[string]any{
			"id": "@telegram_account_a:beeper.local", "fullName": "Account A",
			"providerID": sharedProvider,
		})
	first.ID = "!hostile-a:beeper.local"
	first.SearchAccountID = "account-a"
	second := observationChat("hostile-account", "Telegram",
		"@telegram_account_b:beeper.local", map[string]any{
			"id": "@telegram_account_b:beeper.local", "fullName": "Account B",
			"providerID": sharedProvider,
		})
	second.ID = "!hostile-b:beeper.local"
	second.SearchAccountID = "account-b"
	f.addChat(first)
	f.addChat(second)
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "account-a", NoMedia: true})
	require.NoError(err, "import account-a")
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "account-b", NoMedia: true})
	require.NoError(err, "import account-b")

	left := participantForBeeperUser(t, st, "@telegram_account_a:beeper.local")
	right := participantForBeeperUser(t, st, "@telegram_account_b:beeper.local")
	leftObservations, err := st.ListParticipantObservationsContext(context.Background(), left, true)
	require.NoError(err, "list account-a observations")
	rightObservations, err := st.ListParticipantObservationsContext(context.Background(), right, true)
	require.NoError(err, "list account-b observations")
	require.Len(leftObservations, 1)
	require.Len(rightObservations, 1)
	require.NotNil(leftObservations[0].ProviderUserID)
	require.NotNil(rightObservations[0].ProviderUserID)
	assert.NotEqual(*leftObservations[0].ProviderUserID, *rightObservations[0].ProviderUserID,
		"a hostile chat AccountID must not select the provider-ID namespace")
	assert.False(linked(t, st, left, right),
		"the same native ID in two requested accounts must not auto-link")
}

func TestImportRegistersUnknownBridgeAndPreservesObservedValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("example-bridge", "Example Bridge", "@example-bridge_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{
			"id":       "@example-bridge_alice:beeper.local",
			"fullName": "Alice Example",
			"username": "Alice.Example",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "example-bridge", NoMedia: true})
	require.NoError(err, "Import")

	service, err := st.ResolveCommunicationServiceContext(context.Background(), "example-bridge")
	require.NoError(err, "the unknown bridge must have been registered")
	assert.Equal("Example Bridge", service.DisplayLabel)
	assert.False(service.IsSystem)

	participantID := participantForBeeperUser(t, st, "@example-bridge_alice:beeper.local")
	observations, err := st.ListParticipantObservationsContext(context.Background(), participantID, true)
	require.NoError(err, "ListParticipantObservationsContext")
	require.Len(observations, 1)
	assert.Equal("Alice.Example", observations[0].NormalizedValue,
		"a 'none' strategy must not alter the observed value")
	require.NotNil(observations[0].ScopeValue)
	assert.Equal("account", *observations[0].ScopeKind)
	assert.Equal("example-bridge", *observations[0].ScopeValue)
}

func TestImportClassifiesTheBeeperIdentifierAnchor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("slack", "Slack", "@slack_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{"id": "@slack_alice:beeper.local", "fullName": "Alice Example"},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "slack", NoMedia: true})
	require.NoError(err, "Import")

	slack, err := st.ResolveCommunicationServiceContext(context.Background(), "slack")
	require.NoError(err, "resolve slack")

	var (
		serviceID  int64
		scopeKind  *string
		scopeValue *string
	)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT service_id, scope_kind, scope_value FROM participant_identifiers
		 WHERE identifier_type = ? AND identifier_value = ?`),
		participantIdentifierType,
		providerFallbackUserID("slack", "@slack_alice:beeper.local"),
	).Scan(&serviceID, &scopeKind, &scopeValue), "read the anchor classification")
	assert.Equal(slack.ID, serviceID,
		"PR 3 leaves identifier_type 'beeper' unclassified; the bridge type is only knowable here")
	require.NotNil(scopeKind)
	assert.Equal("account", *scopeKind)
	require.NotNil(scopeValue)
	assert.Equal("slack", *scopeValue)
}

func TestAnchorClassificationRetriesAfterStoreFailureInTheSameRecorder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	userID := "@slack_retry:beeper.local"
	participantID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, userID, "Test User")
	require.NoError(err, "EnsureParticipantByIdentifier")
	cc := captureContext{
		SourceID:  newBeeperTestSource(t, st, "slack"),
		AccountID: "slack",
		Network:   "Slack",
	}
	user := &User{ID: userID, Username: "retry-user"}

	releaseFailure := installAnchorClassificationFailure(t, st)
	_, err = recorder.capture(ctx, participantID, user, cc)
	require.ErrorContains(err, "forced anchor classification failure")
	releaseFailure()

	_, err = recorder.capture(ctx, participantID, user, cc)
	require.NoError(err, "same-recorder retry")

	service, err := st.ResolveCommunicationServiceContext(ctx, "slack")
	require.NoError(err, "resolve slack service")
	var (
		serviceID sql.NullInt64
		scopeKind sql.NullString
		scope     sql.NullString
	)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT service_id, scope_kind, scope_value
		FROM participant_identifiers
		WHERE identifier_type = ? AND identifier_value = ?`),
		participantIdentifierType, userID,
	).Scan(&serviceID, &scopeKind, &scope), "read retried classification")
	assert.True(serviceID.Valid, "a failed classification must not be memoized")
	assert.Equal(service.ID, serviceID.Int64)
	assert.Equal("account", scopeKind.String)
	assert.Equal("slack", scope.String)
}

// installAnchorClassificationFailure makes the real database reject anchor
// classification until the returned release function removes the trigger.
func installAnchorClassificationFailure(t *testing.T, st *store.Store) func() {
	t.Helper()
	require := require.New(t)

	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_beeper_anchor_classification()
			RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'forced anchor classification failure';
			END;
			$$`)
		require.NoError(err, "create PostgreSQL failure function")
		_, err = st.DB().Exec(`CREATE TRIGGER fail_beeper_anchor_classification
			BEFORE UPDATE OF service_id ON participant_identifiers
			FOR EACH ROW EXECUTE FUNCTION fail_beeper_anchor_classification()`)
		require.NoError(err, "create PostgreSQL failure trigger")
		release := func() {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_beeper_anchor_classification
				ON participant_identifiers`)
			require.NoError(err, "drop PostgreSQL failure trigger")
			_, err = st.DB().Exec(`DROP FUNCTION IF EXISTS fail_beeper_anchor_classification()`)
			require.NoError(err, "drop PostgreSQL failure function")
		}
		t.Cleanup(release)
		return release
	}

	_, err := st.DB().Exec(`CREATE TRIGGER fail_beeper_anchor_classification
		BEFORE UPDATE OF service_id ON participant_identifiers
		FOR EACH ROW BEGIN
			SELECT RAISE(ABORT, 'forced anchor classification failure');
		END`)
	require.NoError(err, "create SQLite failure trigger")
	release := func() {
		_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_beeper_anchor_classification`)
		require.NoError(err, "drop SQLite failure trigger")
	}
	t.Cleanup(release)
	return release
}

func TestImportCaptureIsIdempotentAcrossRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{
			"id": "@telegram_alice:beeper.local", "fullName": "Alice Example",
			"username": "@Alice", "phoneNumber": "+12025550123",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	first, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "first Import")
	assert.Equal(int64(3), first.ObservationsRecorded)

	second, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "second Import")
	assert.Equal(int64(0), second.ObservationsRecorded,
		"re-observing the same addresses must create no rows")

	participantID := participantForBeeperUser(t, st, "@telegram_alice:beeper.local")
	all, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err, "ListParticipantObservationsContext")
	assert.Len(all, 2, "history must not grow on a re-import either")
}

func TestImportResetsRunLocalObservationDedupe(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{
			"id": "@telegram_alice:beeper.local", "fullName": "Alice Example",
			"username": "@Alice", "phoneNumber": "+12025550123",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	first, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "first Import")
	require.Equal(int64(3), first.ObservationsRecorded)

	_, err = st.DB().Exec(`DELETE FROM participant_contact_observations`)
	require.NoError(err, "remove observations between import runs")

	second, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "second Import")
	assert.Equal(int64(3), second.ObservationsRecorded,
		"a reusable Importer must start each import with empty run-local dedupe state")
}

func TestImportResetsRunLocalAnchorClassification(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	userID := "@shared_alice:beeper.local"
	participant := map[string]any{"id": userID, "fullName": "Alice Example"}
	self := map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true}
	f.addChat(observationChat("telegram", "Telegram", userID, self, participant))
	f.addChat(observationChat("facebook", "Facebook", userID, self, participant))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	_, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "import Telegram account")
	telegram, err := st.ResolveCommunicationServiceContext(ctx, "telegram")
	require.NoError(err, "resolve Telegram")
	facebook, err := st.ResolveCommunicationServiceContext(ctx, "facebook")
	require.NoError(err, "resolve Facebook")
	readServiceID := func(accountID string) int64 {
		var serviceID int64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT service_id FROM participant_identifiers
			 WHERE identifier_type = ? AND identifier_value = ?`),
			participantIdentifierType, providerFallbackUserID(accountID, userID),
		).Scan(&serviceID), "read anchor service")
		return serviceID
	}
	assert.Equal(telegram.ID, readServiceID("telegram"), "first account classification")

	_, err = imp.Import(ctx, ImportOptions{AccountID: "facebook", NoMedia: true})
	require.NoError(err, "import Facebook account")
	assert.Equal(facebook.ID, readServiceID("facebook"),
		"a reused Importer must not retain the prior account's classified memo")
}

func TestImportUsesParticipantBridgePrefixBeforeOpaqueAccountFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(observationChat(
		"acct-opaque-7", "Mystery Bridge", "@mysterybridge_alice:beeper.local",
		map[string]any{
			"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true,
			"username": "@owner",
		},
		map[string]any{
			"id": "@mysterybridge_alice:beeper.local", "fullName": "Alice Example",
			"username": "@alice",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	_, err := imp.Import(ctx, ImportOptions{AccountID: "acct-opaque-7", NoMedia: true})
	require.NoError(err, "import unknown bridge")
	_, err = st.ResolveCommunicationServiceContext(ctx, "mysterybridge")
	require.NoError(err, "resolve registered bridge prefix")
	_, err = st.ResolveCommunicationServiceContext(ctx, "acct-opaque-7")
	require.ErrorIs(err, store.ErrServiceNotFound,
		"participant ordering must not register the opaque account fallback")

	for _, userID := range []string{"@me:beeper.local", "@mysterybridge_alice:beeper.local"} {
		participantID := participantForBeeperUser(t, st, userID)
		observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
		require.NoError(err, "list observations for %s", userID)
		require.Len(observations, 1)
		require.NotNil(observations[0].ServiceSlug)
		assert.Equal("mysterybridge", *observations[0].ServiceSlug)
	}
}

func TestImportUsesEachParticipantsOwnBridgePrefix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(observationChat(
		"acct-opaque-9", "Mixed Bridge", "@bridge-a_alice:beeper.local",
		map[string]any{
			"id": "@bridge-a_alice:beeper.local", "fullName": "Alice Example",
			"username": "@alice",
		},
		map[string]any{
			"id": "@bridge-b_bob:beeper.local", "fullName": "Bob Example",
			"username": "@bob",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	_, err := imp.Import(ctx, ImportOptions{AccountID: "acct-opaque-9", NoMedia: true})
	require.NoError(err, "import mixed-prefix chat")
	for userID, wantSlug := range map[string]string{
		"@bridge-a_alice:beeper.local": "bridge-a",
		"@bridge-b_bob:beeper.local":   "bridge-b",
	} {
		participantID := participantForBeeperUser(t, st, userID)
		observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
		require.NoError(err, "list observations for %s", userID)
		require.Len(observations, 1)
		require.NotNil(observations[0].ServiceSlug)
		assert.Equal(wantSlug, *observations[0].ServiceSlug)
	}
}

func TestImportDoesNotGuessChatPrefixWhenParticipantsDisagree(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(observationChat(
		"acct-opaque-10", "Mixed Bridge", "@bridge-a_alice:beeper.local",
		map[string]any{
			"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true,
			"username": "@owner",
		},
		map[string]any{
			"id": "@bridge-a_alice:beeper.local", "fullName": "Alice Example",
			"username": "@alice",
		},
		map[string]any{
			"id": "@bridge-b_bob:beeper.local", "fullName": "Bob Example",
			"username": "@bob",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	_, err := imp.Import(ctx, ImportOptions{AccountID: "acct-opaque-10", NoMedia: true})
	require.NoError(err, "import mixed-prefix chat")
	for userID, wantSlug := range map[string]string{
		"@me:beeper.local":             "acct-opaque-10",
		"@bridge-a_alice:beeper.local": "bridge-a",
		"@bridge-b_bob:beeper.local":   "bridge-b",
	} {
		participantID := participantForBeeperUser(t, st, userID)
		observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
		require.NoError(err, "list observations for %s", userID)
		require.Len(observations, 1)
		require.NotNil(observations[0].ServiceSlug)
		assert.Equal(wantSlug, *observations[0].ServiceSlug,
			"an unqualified participant must not inherit an ambiguous bridge prefix")
	}
}

func TestImportDoesNotUsePrefixConsensusFromIncompleteParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{
		ID: "!partial-prefix:beeper.local", AccountID: "acct-partial-prefix",
		Network: "Mixed Bridge", Title: "Partial Prefix", Type: "group",
		ParticipantsTruncated: true, ParticipantListingLimit: 2,
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true, "username": "@owner"},
			{"id": "@bridge-a_alice:beeper.local", "fullName": "Alice Example", "username": "@alice"},
			{"id": "@bridge-b_bob:beeper.local", "fullName": "Bob Example", "username": "@bob"},
		},
		LastActivity: time.Now().UTC(),
	})
	f.setChatGetFailure("!partial-prefix:beeper.local", true)
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	_, err := imp.Import(ctx, ImportOptions{AccountID: "acct-partial-prefix", NoMedia: true})
	require.ErrorContains(err, "partial Beeper sync")
	for userID, wantSlug := range map[string]string{
		"@me:beeper.local":             "acct-partial-prefix",
		"@bridge-a_alice:beeper.local": "bridge-a",
	} {
		participantID := participantForBeeperUser(t, st, userID)
		observations, listErr := st.ListParticipantObservationsContext(ctx, participantID, true)
		require.NoError(listErr, "list observations for %s", userID)
		require.Len(observations, 1)
		require.NotNil(observations[0].ServiceSlug)
		assert.Equal(wantSlug, *observations[0].ServiceSlug,
			"an incomplete member list cannot establish a chat-wide bridge prefix")
	}
}

func TestObservationDedupeKeyIncludesParticipantAndSource(t *testing.T) {
	tests := []struct {
		name            string
		differentSource bool
	}{
		{name: "participant"},
		{name: "source", differentSource: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := context.Background()
			recorder := newObservationRecorder(st)
			firstParticipant, err := st.EnsureParticipantByIdentifier(
				participantIdentifierType, "@telegram_first:beeper.local", "First Person")
			require.NoError(err, "ensure first participant")
			secondParticipant := firstParticipant
			if !test.differentSource {
				secondParticipant, err = st.EnsureParticipantByIdentifier(
					participantIdentifierType, "@telegram_second:beeper.local", "Second Person")
				require.NoError(err, "ensure second participant")
			}
			firstSource := newBeeperTestSource(t, st, "telegram-one")
			secondSource := firstSource
			if test.differentSource {
				secondSource = newBeeperTestSource(t, st, "telegram-two")
			}
			user := &User{ID: "@telegram_shared:beeper.local", Username: "@shared"}

			first, err := recorder.capture(ctx, firstParticipant, user, captureContext{
				SourceID: firstSource, AccountID: "telegram-one", Network: "Telegram",
			})
			require.NoError(err, "first capture")
			require.Len(first, 1)
			second, err := recorder.capture(ctx, secondParticipant, user, captureContext{
				SourceID: secondSource, AccountID: "telegram-two", Network: "Telegram",
			})
			require.NoError(err, "second capture")
			assert.Len(second, 1,
				"the run-local key must not hide another participant or source")
		})
	}
}

func TestImportCapturesTheSelfParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_me:beeper.local",
		map[string]any{
			"id": "@telegram_me:beeper.local", "fullName": "Test User",
			"isSelf": true, "username": "@Owner",
		},
		map[string]any{"id": "@telegram_alice:beeper.local", "fullName": "Alice Example"},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "Import")

	selfID := participantForBeeperUser(t, st, "@telegram_me:beeper.local")
	values := observationValues(t, st, selfID)
	assert.Equal("owner", values[store.ContactAddressUsername],
		"the account owner's own addresses are observations too")
}

func TestSkippedInvalidObservationIsDeduplicatedWithinRecorderRun(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	participantID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@invalid_phone:beeper.local", "Test User")
	requirements.NoError(err, "EnsureParticipantByIdentifier")
	cc := captureContext{
		SourceID:  newBeeperTestSource(t, st, "whatsapp"),
		AccountID: "whatsapp",
		Network:   "WhatsApp",
	}
	user := &User{ID: "@invalid_phone:beeper.local", PhoneNumber: "not-a-phone"}

	first, err := recorder.capture(ctx, participantID, user, cc)
	requirements.NoError(err, "first capture")
	assertions.Empty(first, "an invalid phone is skipped instead of persisted")
	assertions.Len(recorder.seen, 1,
		"the skipped address has no durable row or result, so the run-local key is the observable dedupe state")

	second, err := recorder.capture(ctx, participantID, user, cc)
	requirements.NoError(err, "second capture")
	assertions.Empty(second, "the skipped address remains silent on a repeated capture")
	assertions.Len(recorder.seen, 1, "the second capture must reuse the first skip decision")

	observations, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	requirements.NoError(err, "ListParticipantObservationsContext")
	assertions.Empty(observations, "invalid input must not create an observation while it is deduped")
}

func TestRenamedUsernameSupersedesTheOldValueOnTheSameAnchor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	sourceID := newBeeperTestSource(t, st, "telegram")

	participantID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "EnsureParticipantByIdentifier")
	cc := captureContext{SourceID: sourceID, AccountID: "telegram", Network: "Telegram"}

	_, err = recorder.capture(ctx, participantID, &User{
		ID: "@telegram_alice:beeper.local", FullName: "Alice Example", Username: "@alice_old",
	}, cc)
	require.NoError(err, "first capture")

	// A later run sees the renamed account. A fresh recorder mirrors a fresh
	// process, so the run-local dedupe cache cannot mask the behaviour.
	renamed := newObservationRecorder(st)
	_, err = renamed.capture(ctx, participantID, &User{
		ID: "@telegram_alice:beeper.local", FullName: "Alice Example", Username: "@alice_new",
	}, cc)
	require.NoError(err, "capture after rename")

	current, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err, "current observations")
	require.Len(current, 1, "Beeper exposes one username per network at a time")
	assert.Equal("alice_new", current[0].NormalizedValue)

	all, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err, "all observations")
	require.Len(all, 2, "the earlier username stays on the same participant as history")
}

func TestRenamedUsernameDoesNotSupersedeAnotherSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure participant")
	firstSource := newBeeperTestSource(t, st, "telegram-one")
	secondSource := newBeeperTestSource(t, st, "telegram-two")
	firstAccount := captureContext{
		SourceID: firstSource, AccountID: "telegram-one", Network: "Telegram",
	}
	secondAccount := captureContext{
		SourceID: secondSource, AccountID: "telegram-two", Network: "Telegram",
	}
	sharedProvider := []byte(`{"providerID":"tg-shared"}`)

	_, err = newObservationRecorder(st).capture(ctx, participantID, &User{
		ID: "@telegram_one:beeper.local", Username: "@alice_old", Raw: sharedProvider,
	}, firstAccount)
	require.NoError(err, "capture first account")
	_, err = newObservationRecorder(st).capture(ctx, participantID, &User{
		ID: "@telegram_two:beeper.local", Username: "@alice_other", Raw: sharedProvider,
	}, secondAccount)
	require.NoError(err, "capture second account")
	_, err = newObservationRecorder(st).capture(ctx, participantID, &User{
		ID: "@telegram_one:beeper.local", Username: "@alice_new", Raw: sharedProvider,
	}, firstAccount)
	require.NoError(err, "rename first account")

	current, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err, "list current observations")
	require.Len(current, 2, "a rename must close values only within its source account")
	assert.ElementsMatch([]string{"alice_new", "alice_other"}, []string{
		current[0].NormalizedValue, current[1].NormalizedValue,
	})
	all, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err, "list observation history")
	assert.Len(all, 3)
}

func TestRenamedUsernameSupersessionRetriesAfterPartialWrite(t *testing.T) {
	for _, tc := range []struct {
		name          string
		freshRecorder bool
	}{
		{name: "same recorder"},
		{name: "fresh recorder", freshRecorder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := context.Background()
			recorder := newObservationRecorder(st)
			sourceID := newBeeperTestSource(t, st, "telegram")
			participantID, err := st.EnsureParticipantByIdentifier(
				participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
			require.NoError(err, "EnsureParticipantByIdentifier")
			cc := captureContext{
				SourceID: sourceID, AccountID: "telegram", Network: "Telegram",
			}

			_, err = recorder.capture(ctx, participantID, &User{
				ID: "@telegram_alice:beeper.local", Username: "@alice_old",
			}, cc)
			require.NoError(err, "capture old username")

			releaseFailure := installRenameSupersedeFailure(t, st)
			renamed := &User{
				ID: "@telegram_alice:beeper.local", Username: "@alice_new",
			}
			_, err = recorder.capture(ctx, participantID, renamed, cc)
			require.ErrorContains(err, "forced rename supersede failure")

			current, err := st.ListParticipantObservationsContext(ctx, participantID, true)
			require.NoError(err, "current observations after forced failure")
			require.Len(current, 2, "the new observation commits before supersession fails")
			assert.ElementsMatch([]string{"alice_old", "alice_new"}, []string{
				current[0].NormalizedValue, current[1].NormalizedValue,
			})

			releaseFailure()
			if tc.freshRecorder {
				recorder = newObservationRecorder(st)
			}
			retried, err := recorder.capture(ctx, participantID, renamed, cc)
			require.NoError(err, "retry renamed username")
			require.Len(retried, 1, "the idempotent observation must still run rename repair")
			assert.False(retried[0].Created)

			current, err = st.ListParticipantObservationsContext(ctx, participantID, true)
			require.NoError(err, "current observations after retry")
			require.Len(current, 1, "retry must close the old current username")
			assert.Equal("alice_new", current[0].NormalizedValue)
		})
	}
}

// installRenameSupersedeFailure makes the real database reject observation
// close writes until the returned release function removes the trigger.
func installRenameSupersedeFailure(t *testing.T, st *store.Store) func() {
	t.Helper()
	require := require.New(t)

	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_beeper_username_supersede()
			RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'forced rename supersede failure';
			END;
			$$`)
		require.NoError(err, "create PostgreSQL failure function")
		_, err = st.DB().Exec(`CREATE TRIGGER fail_beeper_username_supersede
			BEFORE UPDATE OF active_until ON participant_contact_observations
			FOR EACH ROW WHEN (NEW.active_until IS NOT NULL)
			EXECUTE FUNCTION fail_beeper_username_supersede()`)
		require.NoError(err, "create PostgreSQL failure trigger")

		release := func() {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_beeper_username_supersede
				ON participant_contact_observations`)
			require.NoError(err, "drop PostgreSQL failure trigger")
			_, err = st.DB().Exec(`DROP FUNCTION IF EXISTS fail_beeper_username_supersede()`)
			require.NoError(err, "drop PostgreSQL failure function")
		}
		t.Cleanup(release)
		return release
	}

	_, err := st.DB().Exec(`CREATE TRIGGER fail_beeper_username_supersede
		BEFORE UPDATE OF active_until ON participant_contact_observations
		FOR EACH ROW WHEN NEW.active_until IS NOT NULL
		BEGIN
			SELECT RAISE(ABORT, 'forced rename supersede failure');
		END`)
	require.NoError(err, "create SQLite failure trigger")
	release := func() {
		_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_beeper_username_supersede`)
		require.NoError(err, "drop SQLite failure trigger")
	}
	t.Cleanup(release)
	return release
}

func TestMultipleUsernamesAndPhonesCoexistOnOneParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)

	participantID, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@alice:beeper.local", "Alice Example")
	require.NoError(err, "EnsureParticipantByIdentifier")

	// The same person seen on two bridged networks, and with a second phone
	// number on one of them.
	_, err = recorder.capture(ctx, participantID, &User{
		ID: "@alice:beeper.local", Username: "@alice_tg", PhoneNumber: "+12025550123",
	}, captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram"),
		AccountID: "telegram", Network: "Telegram",
	})
	require.NoError(err, "telegram capture")
	_, err = recorder.capture(ctx, participantID, &User{
		ID: "@alice:beeper.local", Username: "@alice_x",
	}, captureContext{
		SourceID:  newBeeperTestSource(t, st, "x"),
		AccountID: "x", Network: "X",
	})
	require.NoError(err, "x capture")
	_, err = recorder.capture(ctx, participantID, &User{
		ID: "@alice:beeper.local", Username: "@alice_tg", PhoneNumber: "+12025550199",
	}, captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram"),
		AccountID: "telegram", Network: "Telegram",
	})
	require.NoError(err, "second telegram phone")

	current, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err, "current observations")

	usernames := map[string]bool{}
	phones := map[string]bool{}
	for _, observation := range current {
		switch observation.AddressKind {
		case store.ContactAddressUsername:
			usernames[observation.NormalizedValue] = true
		case store.ContactAddressPhone:
			phones[observation.NormalizedValue] = true
		default:
			// This assertion only partitions the two address kinds seeded above.
		}
	}
	assert.Len(usernames, 2, "two services, two concurrently active usernames")
	assert.True(usernames["alice_tg"])
	assert.True(usernames["alice_x"])
	assert.Len(phones, 2, "a rename never supersedes a phone number")
}

func TestRecycledUsernameDoesNotMergePeopleOrEraseHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	sourceID := newBeeperTestSource(t, st, "telegram")
	cc := captureContext{SourceID: sourceID, AccountID: "telegram", Network: "Telegram"}

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")

	// Alice held the handle, then renamed away from it.
	first := newObservationRecorder(st)
	_, err = first.capture(ctx, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@shared",
	}, cc)
	require.NoError(err, "alice claims the handle")
	_, err = newObservationRecorder(st).capture(ctx, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice_now",
	}, cc)
	require.NoError(err, "alice renames away")

	// Someone else picks the handle up.
	results, err := newObservationRecorder(st).capture(ctx, bob, &User{
		ID: "@telegram_bob:beeper.local", Username: "@shared",
	}, cc)
	require.NoError(err, "bob reuses the handle")
	require.Len(results, 1)
	assert.False(results[0].Conflicting,
		"a superseded handle is history, so reuse is not a live collision")
	assert.Nil(results[0].CandidateID)

	aliceHistory, err := st.ListParticipantObservationsContext(ctx, alice, false)
	require.NoError(err, "alice history")
	assert.Len(aliceHistory, 2, "alice's earlier handle is still recorded")

	members, err := st.ClusterMembers(alice)
	require.NoError(err, "ClusterMembers")
	assert.Equal([]int64{alice}, members, "handle reuse must never merge two people")
	assert.NotContains(members, bob)
}

func TestSameUsernameUnderDifferentStableIDsStaysAConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	cc := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")

	recorder := newObservationRecorder(st)
	_, err = recorder.capture(ctx, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@shared",
	}, cc)
	require.NoError(err, "alice records the handle")

	results, err := recorder.capture(ctx, bob, &User{
		ID: "@telegram_bob:beeper.local", Username: "@shared",
	}, cc)
	require.NoError(err, "bob records the same live handle")
	require.Len(results, 1)
	assert.True(results[0].Conflicting,
		"one live username under two stable IDs is the spec's conflict case")
	require.NotNil(results[0].CandidateID)

	members, err := st.ClusterMembers(alice)
	require.NoError(err, "ClusterMembers")
	assert.Equal([]int64{alice}, members, "a conflict must never merge anyone")

	aliceCurrent, err := st.ListParticipantObservationsContext(ctx, alice, true)
	require.NoError(err, "alice current")
	assert.Len(aliceCurrent, 1, "neither observation is repointed or superseded by the collision")
}
