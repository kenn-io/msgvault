package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func messagingPerson(t *testing.T, st *store.Store, key string) (*store.Person, int64) {
	t.Helper()
	id, err := st.EnsureParticipantByIdentifier("email", key+"@example.test", "Avery Example")
	require.NoError(t, err)
	p, _, err := st.CreatePersonFromParticipant(id)
	require.NoError(t, err)
	return p, id
}
func messagingConversation(t *testing.T, st *store.Store, account, chat, kind string, peer int64) (int64, int64) {
	t.Helper()
	src, err := st.GetOrCreateSource("beeper", account)
	require.NoError(t, err)
	id, err := st.EnsureConversationWithType(src.ID, chat, kind, "Synthetic chat")
	require.NoError(t, err)
	require.NoError(t, st.EnsureConversationParticipant(id, peer, "member"))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET last_sync_at=? WHERE id=?`), time.Now().UTC(), src.ID)
	require.NoError(t, err)
	route := store.MessagingRouteEvidence{ChatID: chat, AccountID: account, Network: "whatsapp", ProviderType: "single", ObservedAt: time.Now().UTC(), MembershipComplete: true, ParticipantIDs: []int64{peer}, SelfParticipantIDs: []int64{}, MemberChatIDs: []string{}}
	writeMessagingEvidence(t, st, id, route)
	return src.ID, id
}
func writeMessagingEvidence(t *testing.T, st *store.Store, id int64, e store.MessagingRouteEvidence) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"messaging_route": e, "member_count": len(e.ParticipantIDs)})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationMetadata(id, sql.NullString{Valid: true, String: string(data)}))
}

func TestMessagingRoutesRespectAccountsTypesAndPagination(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, peer := messagingPerson(t, f.Store, "route-peer")
	_, direct := messagingConversation(t, f.Store, "account-a", "!same:example.test", "direct_chat", peer)
	_, group := messagingConversation(t, f.Store, "account-b", "!same:example.test", "group_chat", peer)
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID, Limit: 1})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 1)
	assertions.Equal(direct, page.Routes.Items[0].ConversationID)
	assertions.Equal("account-a", page.Routes.Items[0].AccountID)
	assertions.Equal("archive_verified", page.Routes.Items[0].Status)
	assertions.True(page.Routes.HasMore)
	next, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID, AfterConversationID: page.Routes.NextAfterID})
	requirements.NoError(err)
	requirements.Len(next.Routes.Items, 1)
	assertions.Equal(group, next.Routes.Items[0].ConversationID)
	assertions.Equal("group_context", next.Routes.Items[0].Status)
	assertions.Equal("archive_only", next.Freshness)
}

func TestMessagingRoutesDoNotTurnCuratedOrSharedPhonesIntoRoutes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, _ := messagingPerson(t, f.Store, "curated-peer")
	other, peer := messagingPerson(t, f.Store, "unrelated-peer")
	for _, p := range []*store.Person{person, other} {
		_, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		requirements.NoError(err)
	}
	messagingConversation(t, f.Store, "shared-account", "!other:example.test", "direct_chat", peer)
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	assertions.Empty(page.Routes.Items)
	requirements.Len(page.ContactPoints.Items, 1)
	assertions.Equal("+12025550123", page.ContactPoints.Items[0].OriginalValue)
}

func TestMessagingRoutesFailClosedOnEvidenceProblems(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		mutate       func(*store.MessagingRouteEvidence)
	}{
		{"incomplete", "membership_incomplete", func(e *store.MessagingRouteEvidence) { e.MembershipComplete = false }},
		{"account mismatch", "account_binding_mismatch", func(e *store.MessagingRouteEvidence) { e.AccountID = "wrong"; e.Failure = "account_binding_mismatch" }},
		{"chat mismatch", "chat_binding_mismatch", func(e *store.MessagingRouteEvidence) { e.ChatID = "wrong" }},
		{"network label only", "network_unverified", func(e *store.MessagingRouteEvidence) {
			e.Network = ""
			e.NetworkLabel = "WhatsApp"
			e.Failure = "network_unverified"
		}},
		{"stale", "evidence_stale", func(e *store.MessagingRouteEvidence) { e.ObservedAt = time.Now().Add(-8 * 24 * time.Hour) }},
		{"interrupted roster", "roster_changed", func(e *store.MessagingRouteEvidence) { e.ParticipantIDs = []int64{999} }},
		{"service failure", "account_lookup_failed", func(e *store.MessagingRouteEvidence) { e.Failure = "account_lookup_failed" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := storetest.New(t)
			p, peer := messagingPerson(t, f.Store, "bad-evidence")
			_, id := messagingConversation(t, f.Store, "account", "!chat:example.test", "direct_chat", peer)
			e := store.MessagingRouteEvidence{ChatID: "!chat:example.test", AccountID: "account", Network: "whatsapp", ProviderType: "single", ObservedAt: time.Now().UTC(), MembershipComplete: true, ParticipantIDs: []int64{peer}}
			test.mutate(&e)
			writeMessagingEvidence(t, f.Store, id, e)
			page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID, Network: "whatsapp"})
			requirements.NoError(err)
			requirements.Len(page.Routes.Items, 1)
			assertions.Equal("unresolved", page.Routes.Items[0].Status)
			assertions.Contains(page.Routes.Items[0].Reasons, test.reason)
			assertions.Len(testutil.MakeSet(page.Routes.Items[0].Reasons...), len(page.Routes.Items[0].Reasons))
		})
	}
}

func TestMessagingRoutesFailClosedWhenMemberCountIsUnknown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, peer := messagingPerson(t, f.Store, "unknown-roster")
	_, conversationID := messagingConversation(t, f.Store, "unknown-roster-account", "!unknown-roster:example.test", "direct_chat", peer)
	requirements.NoError(f.Store.MarkConversationMemberCountUnknown(conversationID))

	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 1)
	assertions.Equal("unresolved", page.Routes.Items[0].Status)
	assertions.False(page.Routes.Items[0].MembershipComplete)
	assertions.Contains(page.Routes.Items[0].Reasons, "membership_incomplete")
}

func TestMessagingRoutesApplySourceAccountLookupFailureToQuietRoutes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, peer := messagingPerson(t, f.Store, "quiet-route")
	sourceID, first := messagingConversation(t, f.Store, "quiet-route-account", "!quiet-one:example.test", "direct_chat", peer)
	_, second := messagingConversation(t, f.Store, "quiet-route-account", "!quiet-two:example.test", "direct_chat", peer)
	requirements.NoError(f.Store.SetSourceMessagingRouteFailureContext(t.Context(), sourceID, "account_lookup_failed"))

	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 2)
	for _, route := range page.Routes.Items {
		assertions.Contains([]int64{first, second}, route.ConversationID)
		assertions.Equal("unresolved", route.Status)
		assertions.Contains(route.Reasons, "account_lookup_failed")
	}
	requirements.NoError(f.Store.SetSourceMessagingRouteFailureContext(t.Context(), sourceID, ""))
	cleared, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	for _, route := range cleared.Routes.Items {
		assertions.Equal("archive_verified", route.Status)
		assertions.NotContains(route.Reasons, "account_lookup_failed")
	}
}

func TestMessagingRoutesUIDAliasesAndErrors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, peer := messagingPerson(t, f.Store, "alias-peer")
	messagingConversation(t, f.Store, "account", "!chat:example.test", "direct_chat", peer)
	_, err := f.Store.RetirePersonUIDAliasContext(t.Context(), "retired-synthetic", &person.ID, "merge")
	requirements.NoError(err)
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: "retired-synthetic"})
	requirements.NoError(err)
	assertions.Equal(person.VCardUID, page.PersonUID)
	assertions.Equal("merge", page.AliasReason)
	_, err = f.Store.RetirePersonUIDAliasContext(t.Context(), "gone-synthetic", nil, "deleted")
	requirements.NoError(err)
	_, err = f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: "gone-synthetic"})
	requirements.ErrorIs(err, store.ErrPersonUIDGone)
	_, err = f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: "missing-synthetic"})
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.GetPersonMessagingRoutesContext(ctx, store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	assertions.ErrorIs(err, context.Canceled)
}

func TestMessagingRoutesSelfOnlyAndMergedContainers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, peer := messagingPerson(t, f.Store, "self-peer")
	_, id := messagingConversation(t, f.Store, "account", "!container:example.test", "direct_chat", peer)
	e := store.MessagingRouteEvidence{ChatID: "!container:example.test", AccountID: "account", Network: "whatsapp", ProviderType: "single", ObservedAt: time.Now().UTC(), MembershipComplete: true, ParticipantIDs: []int64{peer}, SelfParticipantIDs: []int64{peer}}
	writeMessagingEvidence(t, f.Store, id, e)
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 1)
	assertions.Equal("unresolved", page.Routes.Items[0].Status)
	assertions.Contains(page.Routes.Items[0].Reasons, "self_only")
	e.SelfParticipantIDs = nil
	e.Merged = true
	e.MemberChatIDs = []string{"!missing:example.test"}
	writeMessagingEvidence(t, f.Store, id, e)
	page, err = f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 1)
	assertions.Equal("merged_container", page.Routes.Items[0].Status)
	assertions.Equal([]string{"!missing:example.test"}, page.Routes.Items[0].MissingMemberChatIDs)
}

func TestMessagingRoutesRejectInvalidQuery(t *testing.T) {
	t.Parallel()
	f := storetest.New(t)
	for _, q := range []store.PersonMessagingRouteQuery{{}, {PersonUID: "x", Limit: 101}, {PersonUID: "x", AfterObservationID: -1}, {PersonUID: "x", Network: "Whats App"}} {
		_, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), q)
		assert.ErrorIs(t, err, store.ErrInvalidContactLookup, fmt.Sprint(q))
	}
}

func TestMessagingRoutesKeepSuggestionsSeparateAndPageContactEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	person, peer := messagingPerson(t, f.Store, "section-peer")
	other, err := f.Store.EnsureParticipantByIdentifier("email", "suggestion@example.test", "Avery Example")
	requirements.NoError(err)
	for i := range 3 {
		_, err := f.Store.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: fmt.Sprintf("point%d@example.test", i), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		requirements.NoError(err)
		_, err = f.Store.RecordContactObservationContext(t.Context(), peer, store.ParticipantContactObservationInput{AddressKind: store.ContactAddressEmail, OriginalValue: fmt.Sprintf("observation%d@example.test", i), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
		requirements.NoError(err)
	}
	candidate, _, err := f.Store.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: peer, RightKind: store.IdentityMatchParticipant, RightID: other, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation})
	requirements.NoError(err)
	messagingConversation(t, f.Store, "suggested-account", "!suggested:example.test", "direct_chat", other)
	first, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID, Limit: 1})
	requirements.NoError(err)
	assertions.Empty(first.Routes.Items)
	requirements.Len(first.UnreviewedSuggestions.Items, 1)
	assertions.Equal(candidate.ID, first.UnreviewedSuggestions.Items[0].ID)
	assertions.True(first.ContactPoints.HasMore)
	assertions.True(first.Observations.HasMore)
	next, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID, AfterContactPointID: first.ContactPoints.NextAfterID, AfterObservationID: first.Observations.NextAfterID, AfterSuggestionID: first.UnreviewedSuggestions.NextAfterID})
	requirements.NoError(err)
	assertions.Len(next.ContactPoints.Items, 2)
	assertions.Len(next.Observations.Items, 2)
	assertions.Empty(next.UnreviewedSuggestions.Items)
}

func TestMessagingRoutesFollowRealMergeAndSplit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	survivor, left := messagingPerson(t, f.Store, "merge-left")
	absorbed, right := messagingPerson(t, f.Store, "merge-right")
	_, leftChat := messagingConversation(t, f.Store, "left-account", "!left:example.test", "direct_chat", left)
	_, rightChat := messagingConversation(t, f.Store, "right-account", "!right:example.test", "direct_chat", right)
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "route-merge", Actor: "test"})
	requirements.NoError(err)
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: absorbed.VCardUID})
	requirements.NoError(err)
	assertions.Equal(survivor.VCardUID, page.PersonUID)
	assertions.Len(page.Routes.Items, 2)
	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: survivor.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{right}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "route-split", Actor: "test"})
	requirements.NoError(err)
	page, err = f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: absorbed.VCardUID})
	requirements.NoError(err)
	assertions.Equal(split.NewPerson.VCardUID, page.PersonUID)
	requirements.Len(page.Routes.Items, 1)
	assertions.Equal(rightChat, page.Routes.Items[0].ConversationID)
	leftPage, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: survivor.VCardUID})
	requirements.NoError(err)
	requirements.Len(leftPage.Routes.Items, 1)
	assertions.Equal(leftChat, leftPage.Routes.Items[0].ConversationID)
}

func TestMessagingRoutesMissingMetadataStaleSourceAndReadOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	p, peer := messagingPerson(t, f.Store, "read-only-peer")
	src, id := messagingConversation(t, f.Store, "account", "!archive:example.test", "direct_chat", peer)
	requirements.NoError(f.Store.SetConversationMetadata(id, sql.NullString{}))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET last_sync_at=? WHERE id=?`), time.Now().Add(-8*24*time.Hour), src)
	requirements.NoError(err)
	var before, after int64
	if !f.Store.IsPostgreSQL() {
		requirements.NoError(f.Store.DB().QueryRow(`SELECT total_changes()`).Scan(&before))
	}
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 1)
	assertions.Contains(page.Routes.Items[0].Reasons, "route_metadata_missing")
	assertions.Contains(page.Routes.Items[0].Reasons, "source_stale")
	if !f.Store.IsPostgreSQL() {
		requirements.NoError(f.Store.DB().QueryRow(`SELECT total_changes()`).Scan(&after))
	}
	assertions.Equal(before, after)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM sources WHERE id=?`), src)
	requirements.NoError(err)
	page, err = f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID})
	requirements.NoError(err)
	assertions.Empty(page.Routes.Items)
	assertions.Contains(page.Warnings, "no_archived_routes")
}

func TestMessagingRoutesEvidenceBoundsAndFilteredPaging(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	p, peer := messagingPerson(t, f.Store, "bounded-peer")
	_, first := messagingConversation(t, f.Store, "account-a", "!bounded:example.test", "direct_chat", peer)
	secondSource, second := messagingConversation(t, f.Store, "account-b", "!matrix:example.test", "direct_chat", peer)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET source_type='matrix' WHERE id=?`), secondSource)
	requirements.NoError(err)
	requirements.NoError(f.Store.SetConversationMetadata(second, sql.NullString{}))
	unrelatedSource, err := f.Store.GetOrCreateSource("beeper", "unrelated-account")
	requirements.NoError(err)
	filtered, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID, SourceID: unrelatedSource.ID})
	requirements.NoError(err)
	assertions.Empty(filtered.Routes.Items)
	assertions.Contains(filtered.Warnings, "no_routes_on_page")
	assertions.NotContains(filtered.Warnings, "no_archived_routes")
	page, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID, Network: "matrix", Limit: 1})
	requirements.NoError(err)
	assertions.Empty(page.Routes.Items)
	assertions.True(page.Routes.HasMore)
	assertions.Equal(first, page.Routes.NextAfterID)
	assertions.Contains(page.Warnings, "no_routes_on_page")
	next, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID, Network: "matrix", Limit: 1, AfterConversationID: page.Routes.NextAfterID})
	requirements.NoError(err)
	requirements.Len(next.Routes.Items, 1)
	assertions.Equal("matrix", next.Routes.Items[0].Network)
	assertions.Equal("unresolved", next.Routes.Items[0].Status)
	assertions.Contains(next.Routes.Items[0].Reasons, "route_metadata_missing")
	for i := range 101 {
		pid, err := f.Store.EnsureParticipantByIdentifier("email", fmt.Sprintf("roster-%d@example.test", i), "Synthetic Member")
		requirements.NoError(err)
		requirements.NoError(f.Store.EnsureConversationParticipant(first, pid, "member"))
	}
	large, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID})
	requirements.NoError(err)
	requirements.Len(large.Routes.Items, 2)
	assertions.True(large.Routes.Items[0].EvidenceTruncated)
	assertions.Equal("unresolved", large.Routes.Items[0].Status)
	assertions.Contains(large.Routes.Items[0].Reasons, "evidence_truncated")
	requirements.NoError(f.Store.SetConversationMetadata(first, sql.NullString{Valid: true, String: `{"padding":"` + strings.Repeat("🙂", 18000) + `"}`}))
	oversized, err := f.Store.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: p.VCardUID})
	requirements.NoError(err)
	assertions.Contains(oversized.Routes.Items[0].Reasons, "route_metadata_oversized")
}

func TestMessagingRoutesIncludeSuggestionsForBoundCardDAVResource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic-owner", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Synthetic Contacts", CanCreate: new(true)}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	book := books[0]
	input := store.CardDAVRemoteResource{Href: book.CanonicalURL + "avery.vcf", RemoteUID: "route-card-uid", RemoteETag: `"one"`, SemanticHash: "route-card-semantic", DisplayName: "Avery Example", Emails: []string{"avery-card@example.test"}, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:route-card-uid\r\nFN:Avery Example\r\nEMAIL:avery-card@example.test\r\nEND:VCARD\r\n")}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input}})
	requirements.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	requirements.NoError(err)
	requirements.NotNil(resource.PersonID)
	person, err := st.GetPersonContext(t.Context(), *resource.PersonID)
	requirements.NoError(err)
	other, err := st.EnsureParticipantByIdentifier("email", "unreviewed-card@example.test", "Avery Example")
	requirements.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchCardDAVResource, LeftID: resource.ID, RightKind: store.IdentityMatchParticipant, RightID: other, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation})
	requirements.NoError(err)
	messagingConversation(t, st, "card-suggestion-account", "!card-suggested:example.test", "direct_chat", other)
	page, err := st.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.UnreviewedSuggestions.Items, 1)
	assertions.Equal(candidate.ID, page.UnreviewedSuggestions.Items[0].ID)
	assertions.Empty(page.Routes.Items)
}
