package identityops_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type discoveryFakeStore struct {
	identityops.Store

	source            *store.Source
	identities        []store.AccountIdentity
	pages             []store.IdentityDiscoveryPage
	pageCalls         int
	batchCalls        [][]store.IdentityConfirmation
	batchSourceIDs    []int64
	idObservations    []store.IdentityObservation
	scannedSourceID   int64
	scannedMessageIDs []string
	batchFunc         func(context.Context, []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error)
}

func newDiscoveryFakeStore() *discoveryFakeStore {
	return &discoveryFakeStore{source: &store.Source{
		ID: 14, SourceType: "imap", Identifier: "primary@example.test",
	}}
}

func (s *discoveryFakeStore) GetSourceByID(id int64) (*store.Source, error) {
	return s.source, nil
}

func (s *discoveryFakeStore) ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error) {
	return s.identities, nil
}

func (s *discoveryFakeStore) CountIdentityDiscoveryMessagesContext(context.Context, int64) (int64, error) {
	var total int64
	for _, page := range s.pages {
		total += page.Scanned
	}
	return total, nil
}

func (s *discoveryFakeStore) ScanIdentityDiscoveryPageContext(
	context.Context,
	int64,
	int64,
	int,
) (store.IdentityDiscoveryPage, error) {
	if s.pageCalls >= len(s.pages) {
		return store.IdentityDiscoveryPage{}, nil
	}
	page := s.pages[s.pageCalls]
	s.pageCalls++
	return page, nil
}

func (s *discoveryFakeStore) ScanIdentityObservationsForSourceMessageIDsContext(
	_ context.Context,
	sourceID int64,
	sourceMessageIDs []string,
) ([]store.IdentityObservation, error) {
	s.scannedSourceID = sourceID
	s.scannedMessageIDs = append([]string(nil), sourceMessageIDs...)
	return s.idObservations, nil
}

func (s *discoveryFakeStore) AddAccountIdentitiesBatchContext(
	ctx context.Context,
	sourceID int64,
	confirmations []store.IdentityConfirmation,
) ([]store.IdentityConfirmationOutcome, error) {
	s.batchSourceIDs = append(s.batchSourceIDs, sourceID)
	s.batchCalls = append(s.batchCalls, confirmations)
	if s.batchFunc != nil {
		return s.batchFunc(ctx, confirmations)
	}
	outcomes := make([]store.IdentityConfirmationOutcome, len(confirmations))
	for i, confirmation := range confirmations {
		outcomes[i] = store.IdentityConfirmationOutcome{
			Identifier: confirmation.Identifier,
			Added:      true,
			Signals:    confirmation.Signals,
		}
	}
	return outcomes, nil
}

// MergeConfirmedAccountIdentitySignalsContext mirrors the store's merge-only
// boundary: a candidate without a confirmed row is dropped rather than created.
// Writes are recorded alongside the insert-capable batch so tests can assert on
// either path.
func (s *discoveryFakeStore) MergeConfirmedAccountIdentitySignalsContext(
	_ context.Context,
	sourceID int64,
	confirmations []store.IdentityConfirmation,
) ([]store.IdentityConfirmationOutcome, error) {
	confirmed := make(map[string]struct{}, len(s.identities))
	for _, identity := range s.identities {
		confirmed[store.NormalizeIdentifierForCompare(strings.TrimSpace(identity.Address))] = struct{}{}
	}
	outcomes := make([]store.IdentityConfirmationOutcome, 0, len(confirmations))
	merged := make([]store.IdentityConfirmation, 0, len(confirmations))
	for _, confirmation := range confirmations {
		if _, ok := confirmed[store.NormalizeIdentifierForCompare(confirmation.Identifier)]; !ok {
			continue
		}
		merged = append(merged, confirmation)
		outcomes = append(outcomes, store.IdentityConfirmationOutcome{
			Identifier: confirmation.Identifier,
			Added:      false,
			Signals:    confirmation.Signals,
		})
	}
	if len(merged) == 0 {
		return []store.IdentityConfirmationOutcome{}, nil
	}
	s.batchSourceIDs = append(s.batchSourceIDs, sourceID)
	s.batchCalls = append(s.batchCalls, merged)
	return outcomes, nil
}

func classificationsByStableOrder(candidates []identityops.Candidate) []string {
	classifications := make([]string, len(candidates))
	for i, candidate := range candidates {
		classifications[i] = candidate.Classification
	}
	return classifications
}

func TestMergeProviderExternalEvidenceKeepsHistoricalAndReviewsPending(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	result := identityops.DiscoverResult{
		SourceID: 14,
		Candidates: []identityops.Candidate{{
			Identifier:           "Known@Example.test",
			NormalizedIdentifier: "known@example.test",
			Classification:       "confirmed",
			AlreadyConfirmed:     true,
			Signals:              []string{"manual"},
		}},
	}

	identityops.MergeExternalEvidence(&result, []identityops.ExternalEvidence{
		{Identifier: "active@example.test", Signal: identityops.SignalProviderAlias, State: "enabled", Strong: true},
		{Identifier: "old@example.test", Signal: identityops.SignalProviderAlias, State: "disabled", Strong: true},
		{Identifier: "KNOWN@example.test", Signal: identityops.SignalProviderAlias, State: "deleted", Strong: true},
		{Identifier: "waiting@example.test", Signal: identityops.SignalProviderAlias, State: "pending"},
		{Identifier: "*@example.test", Signal: identityops.SignalProviderAlias, State: "enabled", RejectedReason: "wildcard identity"},
		{Identifier: "not an address", Signal: identityops.SignalProviderAlias, State: "enabled", Strong: true},
	})

	requirements.Len(result.Candidates, 4)
	assertions.Equal([]string{"strong", "confirmed", "strong", "weak"}, classificationsByStableOrder(result.Candidates))
	assertions.Equal("Known@Example.test", result.Candidates[1].Identifier, "confirmed spelling must win case folding")
	assertions.Equal([]string{"manual", "provider-alias"}, result.Candidates[1].Signals)
	assertions.True(result.Candidates[1].AlreadyConfirmed)
	assertions.Equal([]identityops.RejectedCandidate{
		{Identifier: "*@example.test", Reason: "wildcard identity"},
		{Identifier: "not an address", Reason: "identifier is not a concrete mailbox address"},
	}, result.Rejected)
}

func TestMergeProviderExternalEvidenceIsDeterministicAcrossInputOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	evidence := []identityops.ExternalEvidence{
		{Identifier: "ALIAS@example.test", Signal: identityops.SignalProviderAlias, State: "pending"},
		{Identifier: "alias@example.test", Signal: identityops.SignalProviderAlias, State: "enabled", Strong: true},
		{Identifier: "Alias@example.test", Signal: "provider-send-as", State: "disabled", Strong: true},
		{Identifier: "other@example.test", Signal: "manual", Strong: true},
	}
	reversed := append([]identityops.ExternalEvidence(nil), evidence...)
	slices.Reverse(reversed)
	left := identityops.DiscoverResult{SourceID: 14}
	right := identityops.DiscoverResult{SourceID: 14}

	identityops.MergeExternalEvidence(&left, evidence)
	identityops.MergeExternalEvidence(&right, reversed)

	assertions.Equal(left, right)
	requirements.Len(left.Candidates, 2)
	assertions.Equal("ALIAS@example.test", left.Candidates[0].Identifier)
	assertions.Equal("strong", left.Candidates[0].Classification)
	assertions.Equal([]string{"provider-alias", "provider-send-as"}, left.Candidates[0].Signals)

	encoded, err := json.Marshal(left)
	requirements.NoError(err)
	var reported struct {
		Candidates []struct {
			Identifier     string   `json:"identifier"`
			ProviderStates []string `json:"provider_states"`
		} `json:"candidates"`
	}
	requirements.NoError(json.Unmarshal(encoded, &reported))
	assertions.Equal([]string{"disabled", "enabled", "pending"}, reported.Candidates[0].ProviderStates)
}

func TestDiscoverWithProviderExternalEvidencePreviewAndApplyUseSameMergedCandidates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	evidence := []identityops.ExternalEvidence{
		{Identifier: "known@example.test", Signal: identityops.SignalProviderAlias, State: "deleted", Strong: true},
		{Identifier: "old@example.test", Signal: identityops.SignalProviderAlias, State: "deleted", Strong: true},
		{Identifier: "waiting@example.test", Signal: identityops.SignalProviderAlias, State: "pending"},
		{Identifier: "OBSERVED@example.test", Signal: identityops.SignalProviderAlias, State: "pending"},
		{Identifier: "observed@example.test", Signal: identityops.SignalProviderAlias, State: "enabled", Strong: true},
	}
	previewStore := newDiscoveryFakeStore()
	previewStore.identities = []store.AccountIdentity{{
		SourceID: 14, Address: "Known@Example.test", SourceSignal: "manual",
	}}
	previewStore.pages = []store.IdentityDiscoveryPage{{
		Scanned: 1, NextAfterID: 1,
		Observations: []store.IdentityObservation{{
			MessageID: 1, Identifier: "observed@example.test", RecipientType: "from", HasSentFolder: true,
		}},
	}}

	preview, err := identityops.DiscoverWithExternalEvidence(t.Context(), previewStore, identityops.DiscoverRequest{
		SourceID: 14,
	}, evidence, nil)
	requirements.NoError(err)

	applyStore := newDiscoveryFakeStore()
	applyStore.pages = previewStore.pages
	applyStore.identities = previewStore.identities
	apply, err := identityops.DiscoverWithExternalEvidence(t.Context(), applyStore, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, evidence, nil)
	requirements.NoError(err)

	assertions.Equal(preview.Candidates, apply.Candidates)
	requirements.Len(preview.Candidates, 4)
	assertions.Equal("Known@Example.test", preview.Candidates[0].Identifier)
	assertions.Equal("confirmed", preview.Candidates[0].Classification)
	assertions.True(preview.Candidates[0].AlreadyConfirmed)
	assertions.Equal([]string{"manual", "provider-alias"}, preview.Candidates[0].Signals)
	encoded, err := json.Marshal(preview)
	requirements.NoError(err)
	var reported struct {
		Candidates []struct {
			Identifier     string   `json:"identifier"`
			Signals        []string `json:"signals"`
			ProviderStates []string `json:"provider_states"`
		} `json:"candidates"`
	}
	requirements.NoError(json.Unmarshal(encoded, &reported))
	assertions.Equal([]string{"deleted"}, reported.Candidates[0].ProviderStates,
		"provider-only confirmed identity retains its inventory state")
	assertions.Equal("observed@example.test", reported.Candidates[1].Identifier,
		"archive observation spelling wins case-folded provider duplicates")
	assertions.Equal([]string{"provider-alias", "sent-folder"}, reported.Candidates[1].Signals)
	assertions.Equal([]string{"enabled", "pending"}, reported.Candidates[1].ProviderStates)
	requirements.Len(applyStore.batchCalls, 1)
	assertions.Equal([]store.IdentityConfirmation{
		{Identifier: "Known@Example.test", Signals: []string{"provider-alias"}},
		{Identifier: "observed@example.test", Signals: []string{"provider-alias", "sent-folder"}},
		{Identifier: "old@example.test", Signals: []string{"provider-alias"}},
	}, applyStore.batchCalls[0], "pending provider evidence must remain preview-only")
}

func TestApplyProviderExternalEvidenceUsesBoundedStoreServiceAndIsRetryIdempotent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("imap", "other@example.test")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(other.ID, "other-alias@example.test", "manual"))
	evidence := []identityops.ExternalEvidence{
		{Identifier: "active@example.test", Signal: identityops.SignalProviderAlias, State: "enabled", Strong: true},
		{Identifier: "old@example.test", Signal: identityops.SignalProviderAlias, State: "disabled", Strong: true},
		{Identifier: "deleted@example.test", Signal: identityops.SignalProviderAlias, State: "deleted", Strong: true},
		{Identifier: "waiting@example.test", Signal: identityops.SignalProviderAlias, State: "pending"},
		{Identifier: "*@example.test", Signal: identityops.SignalProviderAlias, State: "enabled", Strong: true},
	}
	beforeRevision, err := st.AccountIdentityRevision()
	requirements.NoError(err)

	first, err := identityops.ApplyExternalEvidence(t.Context(), st, source.ID, evidence)
	requirements.NoError(err)
	assertions.Equal([]string{
		"active@example.test", "deleted@example.test", "old@example.test",
	}, identityConfirmationIdentifiers(first))
	afterFirstRevision, err := st.AccountIdentityRevision()
	requirements.NoError(err)
	assertions.Equal(beforeRevision+1, afterFirstRevision)

	retry, err := identityops.ApplyExternalEvidence(t.Context(), st, source.ID, evidence)
	requirements.NoError(err)
	assertions.Empty(retry)
	afterRetryRevision, err := st.AccountIdentityRevision()
	requirements.NoError(err)
	assertions.Equal(afterFirstRevision, afterRetryRevision)

	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	assertions.Equal([]string{
		"active@example.test", "deleted@example.test", "old@example.test",
	}, accountIdentityAddresses(identities))
	otherIdentities, err := st.ListAccountIdentities(other.ID)
	requirements.NoError(err)
	assertions.Equal([]string{"other-alias@example.test"}, accountIdentityAddresses(otherIdentities))
}

func identityConfirmationIdentifiers(outcomes []store.IdentityConfirmationOutcome) []string {
	identifiers := make([]string, len(outcomes))
	for i, outcome := range outcomes {
		identifiers[i] = outcome.Identifier
	}
	return identifiers
}

func accountIdentityAddresses(identities []store.AccountIdentity) []string {
	addresses := make([]string, len(identities))
	for i, identity := range identities {
		addresses[i] = identity.Address
	}
	return addresses
}

func TestDiscoverClassifiesStrongWeakConfirmedAndCaseVariants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.identities = []store.AccountIdentity{{
		SourceID: 14, Address: "Known@Example.test", SourceSignal: "manual",
	}}
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned:     4,
		NextAfterID: 4,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "masked-shop@example.test", RecipientType: "from", HasSentFolder: true, ObservedAt: time.Unix(10, 0).UTC()},
			{MessageID: 2, Identifier: "MASKED-SHOP@example.test", RecipientType: "to", ObservedAt: time.Unix(20, 0).UTC()},
			{MessageID: 3, Identifier: "Known@example.test", RecipientType: "from", IsFromMe: true, ObservedAt: time.Unix(30, 0).UTC()},
			{MessageID: 4, Identifier: "list@example.test", RecipientType: "cc", ObservedAt: time.Unix(40, 0).UTC()},
		},
	}}

	got, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
	}, nil)
	require.NoError(err)
	require.Len(got.Candidates, 3)
	assert.Equal([]string{"confirmed", "weak", "strong"}, classificationsByStableOrder(got.Candidates))
	assert.Equal([]string{"is_from_me", "manual"}, got.Candidates[0].Signals)
	assert.Equal("masked-shop@example.test", got.Candidates[2].Identifier, "first spelling must win")
	assert.Equal(int64(1), got.Candidates[2].SentMessageCount)
	assert.Equal(int64(1), got.Candidates[2].ReceivedMessageCount)
}

func TestDiscoverApplyNeverConfirmsRecipientOnlyCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned:     2,
		NextAfterID: 2,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "masked-shop@example.test", RecipientType: "from", HasSentFolder: true},
			{MessageID: 2, Identifier: "list@example.test", RecipientType: "to"},
		},
	}}

	_, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
		Apply:    true,
	}, nil)
	require.NoError(err)
	assert.Equal([]int64{14}, st.batchSourceIDs)
	require.Len(st.batchCalls, 1)
	require.Len(st.batchCalls[0], 1)
	assert.Equal("masked-shop@example.test", st.batchCalls[0][0].Identifier)
	assert.Equal([]string{"sent-folder"}, st.batchCalls[0][0].Signals)
}

func TestDiscoverApplyDoesNotConfirmAmbiguousFromAddresses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned:     1,
		NextAfterID: 1,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "author@example.test", RecipientType: "from", IsFromMe: true, HasSentFolder: true, HasSentLabel: true},
			{MessageID: 1, Identifier: "coauthor@example.test", RecipientType: "from", IsFromMe: true, HasSentFolder: true, HasSentLabel: true},
		},
	}}

	got, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
		Apply:    true,
	}, nil)
	require.NoError(err)
	require.Len(got.Candidates, 2)
	for _, candidate := range got.Candidates {
		assert.Equal("weak", candidate.Classification)
		assert.Empty(candidate.Signals)
		assert.Equal(int64(1), candidate.SentMessageCount)
	}
	assert.Empty(got.Applied)
	assert.Empty(st.batchCalls)
}

func TestDiscoverApplyMergesOnlyNewStrongSignalsIntoConfirmedCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.identities = []store.AccountIdentity{{
		SourceID: 14, Address: "Known@Example.test", SourceSignal: "manual,sent-folder",
	}}
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned: 1, NextAfterID: 1,
		Observations: []store.IdentityObservation{{
			MessageID: 1, Identifier: "known@example.test", RecipientType: "from",
			IsFromMe: true, HasSentFolder: true,
		}},
	}}

	_, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, nil)
	require.NoError(err)
	assert.Equal([]int64{14}, st.batchSourceIDs)
	require.Len(st.batchCalls, 1)
	require.Len(st.batchCalls[0], 1)
	assert.Equal("known@example.test", st.batchCalls[0][0].Identifier)
	assert.Equal([]string{"is_from_me"}, st.batchCalls[0][0].Signals)

	st = newDiscoveryFakeStore()
	st.identities = []store.AccountIdentity{{
		SourceID: 14, Address: "known@example.test", SourceSignal: "is_from_me",
	}}
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned: 1, NextAfterID: 1,
		Observations: []store.IdentityObservation{{
			MessageID: 1, Identifier: "KNOWN@example.test", RecipientType: "from", IsFromMe: true,
		}},
	}}
	_, err = identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, nil)
	require.NoError(err)
	assert.Empty(st.batchCalls, "unchanged evidence must not start an empty write batch")
}

func TestDiscoverExplicitlyConfirmsOnlyCompletedWeakCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned:     2,
		NextAfterID: 2,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "weak@example.test", RecipientType: "to"},
			{MessageID: 2, Identifier: "strong@example.test", RecipientType: "from", IsFromMe: true},
		},
	}}

	got, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
		Apply:    true,
		Confirm:  []string{"WEAK@example.test", "weak@example.test"},
	}, nil)
	require.NoError(err)
	require.Len(got.Applied, 2)
	assert.Equal("strong@example.test", got.Applied[0].Identifier)
	assert.Equal([]string{"is_from_me"}, got.Applied[0].Signals)
	assert.Equal("weak@example.test", got.Applied[1].Identifier)
	assert.Equal([]string{"manual"}, got.Applied[1].Signals)

	st = newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned:     1,
		NextAfterID: 1,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "strong@example.test", RecipientType: "from", IsFromMe: true},
		},
	}}
	_, err = identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
		Apply:    true,
		Confirm:  []string{"strong@example.test"},
	}, nil)
	require.ErrorContains(err, "weak candidate")
	assert.Empty(st.batchCalls, "all explicit confirmations must validate before writing")
}

func TestDiscoverConfirmRequiresApply(t *testing.T) {
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned: 1, NextAfterID: 1,
		Observations: []store.IdentityObservation{{
			MessageID: 1, Identifier: "weak@example.test", RecipientType: "to",
		}},
	}}

	_, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
		Confirm:  []string{"weak@example.test"},
	}, nil)
	require.ErrorContains(t, err, "requires apply")
	assert.Empty(t, st.batchCalls)
}

func TestDiscoverStrongForSourceMessageIDsAppliesOnlyMergedStrongEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.identities = []store.AccountIdentity{
		{SourceID: 14, Address: "alias@example.test", SourceSignal: "manual"},
		{SourceID: 14, Address: "recipient@example.test", SourceSignal: "manual"},
	}
	st.idObservations = []store.IdentityObservation{
		{MessageID: 3, Identifier: "Alias@Example.test", RecipientType: "from", HasSentFolder: true},
		{MessageID: 4, Identifier: "alias@example.test", RecipientType: "from", IsFromMe: true, HasSentLabel: true},
		{MessageID: 4, Identifier: "recipient@example.test", RecipientType: "to", IsFromMe: true, HasSentLabel: true},
	}

	got, err := identityops.DiscoverStrongForSourceMessageIDs(
		t.Context(), st, 14, []string{"provider-2", "provider-1", "provider-2"},
	)
	require.NoError(err)
	assert.Equal(int64(14), st.scannedSourceID)
	assert.Equal([]string{"provider-2", "provider-1", "provider-2"}, st.scannedMessageIDs)
	assert.Equal([]int64{14}, st.batchSourceIDs)
	require.Len(got, 1)
	assert.Equal("Alias@Example.test", got[0].Identifier)
	assert.Equal([]string{"is_from_me", "sent-folder", "sent-label"}, got[0].Signals)
}

// newSentEvidenceStore returns a real store holding one Gmail source, so the
// refresh-only tests below exercise the production discovery scan rather than a
// hand-built observation list.
func newSentEvidenceStore(t *testing.T) (*store.Store, *store.Source) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "primary@example.test")
	require.NoError(t, err, "create source")
	return st, source
}

// archiveSentMessage archives one Sent-labelled message carrying
// provider-native attribution whose only From address is sender.
func archiveSentMessage(t *testing.T, st *store.Store, source *store.Source, sourceMessageID, sender string) {
	t.Helper()
	convID, err := st.EnsureConversation(source.ID, "thread-"+sourceMessageID, "Thread")
	require.NoError(t, err, "create conversation")
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: sourceMessageID,
		MessageType:     "email",
		IsFromMe:        true,
		SizeEstimate:    100,
	})
	require.NoError(t, err, "archive message")
	participantID, err := st.EnsureParticipant(sender, "Test User", "example.test")
	require.NoError(t, err, "create participant")
	require.NoError(t,
		st.ReplaceMessageRecipients(messageID, "from", []int64{participantID}, []string{"Test User"}),
		"add From participant")
	sentLabelID, err := st.EnsureLabel(source.ID, "SENT", "Sent Mail", "system")
	require.NoError(t, err, "create Gmail sent label")
	require.NoError(t, st.ReplaceMessageLabels(messageID, []int64{sentLabelID}), "label sent message")
}

func TestDiscoverStrongForSourceMessageIDsNeverConfirmsFirstTimeIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := newSentEvidenceStore(t)
	archiveSentMessage(t, st, source, "fresh-sent", "newcomer@example.test")

	outcomes, err := identityops.DiscoverStrongForSourceMessageIDs(
		t.Context(), st, source.ID, []string{"fresh-sent"},
	)
	require.NoError(err)
	assert.Empty(outcomes, "sync-time discovery must not confirm a first-time identity")

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(identities, "a single Sent-placed message must not create an identity")
}

func TestDiscoverStrongForSourceMessageIDsMergesSignalsIntoConfirmed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := newSentEvidenceStore(t)
	require.NoError(st.AddAccountIdentity(source.ID, "owner@example.test", "manual"), "pre-confirm identity")
	archiveSentMessage(t, st, source, "confirmed-sent", "owner@example.test")

	_, err := identityops.DiscoverStrongForSourceMessageIDs(
		t.Context(), st, source.ID, []string{"confirmed-sent"},
	)
	require.NoError(err)

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("owner@example.test", identities[0].Address)
	assert.Equal([]string{"is_from_me", "manual", "sent-label"},
		identityops.SplitSignalSet(identities[0].SourceSignal),
		"new strong signals must merge into the already-confirmed identity")
}

func TestDiscoverStrongForSourceMessageIDsDoesNotReAddRemovedIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := newSentEvidenceStore(t)
	require.NoError(st.AddAccountIdentity(source.ID, "retired@example.test", "manual"), "confirm identity")
	removed, err := st.RemoveAccountIdentity(source.ID, "retired@example.test")
	require.NoError(err, "remove identity")
	require.Equal(int64(1), removed)
	archiveSentMessage(t, st, source, "post-removal-sent", "retired@example.test")

	_, err = identityops.DiscoverStrongForSourceMessageIDs(
		t.Context(), st, source.ID, []string{"post-removal-sent"},
	)
	require.NoError(err)

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(identities, "sync-time discovery must not resurrect a removed identity")
}

func TestRefreshConfirmedForSourceMergesOnlyConfirmed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := newSentEvidenceStore(t)
	require.NoError(st.AddAccountIdentity(source.ID, "owner@example.test", "manual"), "pre-confirm identity")
	archiveSentMessage(t, st, source, "confirmed-sent", "owner@example.test")
	archiveSentMessage(t, st, source, "unconfirmed-sent", "stranger@example.test")

	require.NoError(identityops.RefreshConfirmedForSource(t.Context(), st, source.ID))

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1, "refresh must never confirm an unconfirmed address")
	assert.Equal("owner@example.test", identities[0].Address)
	assert.Equal([]string{"is_from_me", "manual", "sent-label"},
		identityops.SplitSignalSet(identities[0].SourceSignal),
		"refresh must merge full-archive strong signals into the confirmed identity")
}

func TestRefreshConfirmedForSourceSkipsScanWithoutConfirmedIdentities(t *testing.T) {
	assert := assert.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned: 1, NextAfterID: 1,
		Observations: []store.IdentityObservation{{
			MessageID: 1, Identifier: "stranger@example.test", RecipientType: "from", IsFromMe: true,
		}},
	}}

	require.NoError(t, identityops.RefreshConfirmedForSource(t.Context(), st, 14))
	assert.Zero(st.pageCalls, "a source with no confirmed identities must not be scanned")
	assert.Empty(st.batchCalls)
}

func TestDiscoverRejectsUnsafeAddressesAndCountsDistinctMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned:     3,
		NextAfterID: 3,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: " Alias@Example.test ", RecipientType: "from", HasSentFolder: true, ObservedAt: time.Unix(30, 0).UTC()},
			{MessageID: 1, Identifier: "alias@example.test", RecipientType: "from", IsFromMe: true, ObservedAt: time.Unix(20, 0).UTC()},
			{MessageID: 2, Identifier: "ALIAS@example.test", RecipientType: "to", ObservedAt: time.Unix(40, 0).UTC()},
			{MessageID: 2, Identifier: "alias@example.test", RecipientType: "cc", ObservedAt: time.Unix(40, 0).UTC()},
			{MessageID: 3, Identifier: "Display Name <display@example.test>", RecipientType: "from", IsFromMe: true},
			{MessageID: 3, Identifier: "*@example.test", RecipientType: "from", IsFromMe: true},
			{MessageID: 3, Identifier: "line@example.test\n", RecipientType: "from", IsFromMe: true},
			{MessageID: 3, Identifier: "user@localhost", RecipientType: "from", IsFromMe: true},
			{MessageID: 3, Identifier: "@Synthetic:example.test", RecipientType: "from", IsFromMe: true},
			{MessageID: 3, Identifier: "@synthetic:example.test", RecipientType: "from", IsFromMe: true},
		},
	}}

	got, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14,
	}, nil)
	require.NoError(err)
	require.Len(got.Candidates, 1)
	assert.Equal("Alias@Example.test", got.Candidates[0].Identifier)
	assert.Equal(int64(1), got.Candidates[0].SentMessageCount)
	assert.Equal(int64(1), got.Candidates[0].ReceivedMessageCount)
	assert.Equal(time.Unix(20, 0).UTC(), got.Candidates[0].FirstSeenAt)
	assert.Equal(time.Unix(40, 0).UTC(), got.Candidates[0].LastSeenAt)
	assert.Len(got.Rejected, 6)
	assert.Contains(got.Rejected, identityops.RejectedCandidate{
		Identifier: "user@localhost",
		Reason:     "identifier is not a concrete mailbox address",
	})
	assert.Contains(rejectedIdentifiers(got.Rejected), "@Synthetic:example.test")
	assert.Contains(rejectedIdentifiers(got.Rejected), "@synthetic:example.test")
}

func TestDiscoverCancellationAfterPageWritesNothing(t *testing.T) {
	assert := assert.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{
		{
			Scanned: 1, NextAfterID: 1,
			Observations: []store.IdentityObservation{{
				MessageID: 1, Identifier: "first@example.test", RecipientType: "from", IsFromMe: true,
			}},
		},
		{
			Scanned: 1, NextAfterID: 2,
			Observations: []store.IdentityObservation{{
				MessageID: 2, Identifier: "second@example.test", RecipientType: "from", IsFromMe: true,
			}},
		},
	}
	ctx, cancel := context.WithCancel(t.Context())

	got, err := identityops.Discover(ctx, st, identityops.DiscoverRequest{
		SourceID: 14, Apply: true, PageSize: 1,
	}, func(identityops.DiscoverProgress) error {
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(identityops.DiscoverResult{}, got, "canceled scan must not return a preview result")
	assert.Empty(st.batchCalls)
}

func TestDiscoverInterruptedApplyRerunMatchesOneShot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	pages := []store.IdentityDiscoveryPage{{
		Scanned: 2, NextAfterID: 2,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "first@example.test", RecipientType: "from", IsFromMe: true},
			{MessageID: 2, Identifier: "second@example.test", RecipientType: "from", HasSentFolder: true},
		},
	}}

	resumedState := make(map[string]map[string]struct{})
	resumed := newDiscoveryFakeStore()
	resumed.pages = pages
	resumed.batchFunc = func(_ context.Context, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error) {
		outcomes := mergeConfirmationState(resumedState, confirmations[:1])
		return outcomes, context.Canceled
	}
	_, err := identityops.Discover(t.Context(), resumed, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, nil)
	require.ErrorIs(err, context.Canceled)

	resumed.pageCalls = 0
	resumed.batchFunc = func(_ context.Context, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error) {
		return mergeConfirmationState(resumedState, confirmations), nil
	}
	_, err = identityops.Discover(t.Context(), resumed, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, nil)
	require.NoError(err)

	cleanState := make(map[string]map[string]struct{})
	clean := newDiscoveryFakeStore()
	clean.pages = pages
	clean.batchFunc = func(_ context.Context, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error) {
		return mergeConfirmationState(cleanState, confirmations), nil
	}
	_, err = identityops.Discover(t.Context(), clean, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, nil)
	require.NoError(err)
	assert.Equal(cleanState, resumedState)
}

func TestDiscoverApplyErrorReturnsCommittedPrefix(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newDiscoveryFakeStore()
	st.pages = []store.IdentityDiscoveryPage{{
		Scanned: 2, NextAfterID: 2,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "first@example.test", RecipientType: "from", IsFromMe: true},
			{MessageID: 2, Identifier: "second@example.test", RecipientType: "from", HasSentFolder: true},
		},
	}}
	applyErr := errors.New("apply stopped after committed prefix")
	st.batchFunc = func(_ context.Context, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error) {
		return []store.IdentityConfirmationOutcome{{
			Identifier: confirmations[0].Identifier,
			Added:      true,
			Signals:    append([]string(nil), confirmations[0].Signals...),
		}}, applyErr
	}

	got, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: 14, Apply: true,
	}, nil)

	requirements.ErrorIs(err, applyErr)
	requirements.Len(got.Applied, 1)
	assertions.Equal("first@example.test", got.Applied[0].Identifier)
	assertions.True(got.Applied[0].Added)
	assertions.Equal([]string{"is_from_me"}, got.Applied[0].Signals)
}

func rejectedIdentifiers(rejected []identityops.RejectedCandidate) []string {
	identifiers := make([]string, len(rejected))
	for i, candidate := range rejected {
		identifiers[i] = candidate.Identifier
	}
	return identifiers
}

func mergeConfirmationState(
	state map[string]map[string]struct{},
	confirmations []store.IdentityConfirmation,
) []store.IdentityConfirmationOutcome {
	outcomes := make([]store.IdentityConfirmationOutcome, 0, len(confirmations))
	for _, confirmation := range confirmations {
		key := strings.ToLower(confirmation.Identifier)
		if state[key] == nil {
			state[key] = make(map[string]struct{})
		}
		for _, signal := range confirmation.Signals {
			state[key][signal] = struct{}{}
		}
		signals := make([]string, 0, len(state[key]))
		for signal := range state[key] {
			signals = append(signals, signal)
		}
		sort.Strings(signals)
		outcomes = append(outcomes, store.IdentityConfirmationOutcome{
			Identifier: confirmation.Identifier, Signals: signals,
		})
	}
	return outcomes
}

func TestDiscoverApplyAfterParticipantMergeConfirmsOnlyEnvelopeAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err, "create source")
	convID, err := st.EnsureConversation(source.ID, "merge-thread", "Merge Thread")
	require.NoError(err, "create conversation")
	aliceID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(err, "create alice")
	bobID, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(err, "create bob")

	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID:  convID,
			SourceID:        source.ID,
			SourceMessageID: "native-sent-from-alice",
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), Valid: true},
			SenderID:        sql.NullInt64{Int64: aliceID, Valid: true},
			IsFromMe:        true,
			SizeEstimate:    100,
		},
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{aliceID},
			DisplayNames:   []string{"Alice"},
			EmailAddresses: []string{"alice@example.test"},
		}},
	})
	require.NoError(err, "persist source-native sent message")
	require.NoError(st.MergeParticipants(aliceID, bobID), "merge alice into bob")

	result, err := identityops.Discover(t.Context(), st, identityops.DiscoverRequest{
		SourceID: source.ID,
		Apply:    true,
	}, nil)
	require.NoError(err, "discover with apply")

	for _, candidate := range result.Candidates {
		if candidate.NormalizedIdentifier != "bob@example.test" {
			continue
		}
		assert.NotEqual("strong", candidate.Classification,
			"the merge survivor's primary email must not become strong evidence")
		assert.False(candidate.AlreadyConfirmed,
			"apply must not confirm an address the provider never emitted as the sender")
	}

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err, "list confirmed identities")
	require.Len(identities, 1, "apply must confirm exactly the envelope address")
	assert.Equal("alice@example.test", identities[0].Address)
	assert.Equal("is_from_me", identities[0].SourceSignal)
}
