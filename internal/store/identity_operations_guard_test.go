package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func TestIdentityOperationPreviewRejectsUnsafeDerivedIDs(t *testing.T) {
	const unsafeID = int64(9_007_199_254_740_992)
	for _, kind := range []string{"participant", "source", "person"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)

			st, a, b := newUnlinkGuardStore(t)
			override := ""
			if st.IsPostgreSQL() {
				override = "OVERRIDING SYSTEM VALUE"
			}
			switch kind {
			case "participant":
				// A legacy archive can contain IDs beyond JSON's exact integer range.
				// Seed that row, then exercise the real native graph writer and preview.
				_, err := st.db.Exec(fmt.Sprintf(`INSERT INTO participants(id,email_address,display_name) %s VALUES (?,?,?)`, override), unsafeID, "legacy-unsafe@example.test", "Synthetic Legacy")
				requirements.NoError(err)
				_, err = st.LinkParticipants(a, unsafeID)
				requirements.NoError(err)
			case "source":
				_, err := st.db.Exec(fmt.Sprintf(`INSERT INTO sources(id,source_type,identifier) %s VALUES (?,?,?)`, override), unsafeID, "gmail", "legacy-source@example.test")
				requirements.NoError(err)
				conversation, err := st.EnsureConversation(unsafeID, "legacy-source-thread", "Synthetic Thread")
				requirements.NoError(err)
				_, err = st.UpsertMessage(&Message{SourceID: unsafeID, ConversationID: conversation, SourceMessageID: "legacy-source-message", MessageType: "email", SenderID: sql.NullInt64{Int64: a, Valid: true}})
				requirements.NoError(err)
			case "person":
				_, err := st.db.Exec(fmt.Sprintf(`INSERT INTO persons(id,vcard_uid,display_name) %s VALUES (?,?,?)`, override), unsafeID, "urn:uuid:00000000-0000-4000-8000-000000000009", "Synthetic Legacy Person")
				requirements.NoError(err)
				requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
					if err := st.lockIdentityMutationTxContext(t.Context(), tx); err != nil {
						return err
					}
					_, err := st.bindPersonParticipantsTx(t.Context(), tx, unsafeID, []int64{a})
					return err
				}))
			}
			snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
			requirements.ErrorIs(err, identitycontrol.ErrInvalidRequest)
			assert.Nil(t, snapshot, "derived archive IDs must remain exact at the protocol boundary")
		})
	}
}

func TestIdentityOperationPreviewIncludesPersonCandidateConflicts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	otherParticipant, err := st.EnsureParticipant("other-curated@example.test", "Other Synthetic Person", "example.test")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchPerson, LeftID: target.ID, RightKind: IdentityMatchPerson, RightID: other.ID,
		Basis: IdentityMatchEmail, NormalizedValue: new("curated-conflict@example.test"), State: IdentityMatchStateConflict, Source: ProvenanceArchiveObservation,
	})
	requirements.NoError(err)
	binding, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: a, PersonID: target.ID})
	requirements.NoError(err)
	assertions.Contains(binding.Blockers, "conflicting-identity-evidence")
	var candidateIDs []int64
	for _, evidence := range binding.Candidates {
		candidateIDs = append(candidateIDs, evidence.ID)
	}
	assertions.Contains(candidateIDs, candidate.ID)
	graph, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	requirements.NoError(err)
	assertions.NotContains(graph.Blockers, "conflicting-identity-evidence", "direct-binding limitation does not change existing graph semantics")
}

func TestIdentityOperationPreviewRetainsUnresolvedCardDAVConflict(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic/conflict/"
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/synthetic/", HomeURL: "https://contacts.example.test/books/synthetic/",
		Books: []CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Conflict Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	book := books[0]
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
	requirements.NoError(err)
	requirements.Len(books, 1)
	book = books[0]
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Synthetic Person\r\nEND:VCARD\r\n")
	remote := CardDAVRemoteResource{Href: bookURL + "synthetic.vcf", RemoteUID: person.VCardUID, RemoteETag: `"synthetic-one"`, RemoteBody: body, SemanticHash: "synthetic-one", DisplayName: "Synthetic Person"}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []CardDAVRemoteResource{remote}})
	requirements.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	requirements.NoError(err)
	requirements.Equal(&person.ID, resource.PersonID)
	target := identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}
	before, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink, target)
	requirements.NoError(err)
	assertions.NotContains(before.Blockers, "carddav-publication")
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), CardDAVConflictCapture{AddressBookID: book.ID, Href: remote.Href, ExpectedMappingRevision: resource.MappingRevision, BaseLocalHash: resource.LocalHash, LocalHash: resource.LocalHash, BaseRemoteHash: resource.RemoteSemanticHash, BaseRemoteETag: resource.RemoteETag, RemoteETag: `"synthetic-two"`, LocalBody: body, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Changed Synthetic Person\r\nEND:VCARD\r\n")})
	requirements.NoError(err)
	assertions.Empty(conflict.LocalMutationIntent, "this conflict has no publication intent, so it exercises the additional binding guard")
	after, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink, target)
	requirements.NoError(err)
	assertions.Contains(after.Blockers, "carddav-publication")
	assertions.NotEqual(before.Fingerprint, after.Fingerprint)
	var publications int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM carddav_publications WHERE person_id=?`, person.ID).Scan(&publications))
	assertions.Zero(publications, "preview must not create a publication as a workaround")
	current, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	requirements.NoError(err)
	assertions.Equal(CardDAVConflictUnresolved, current.Status)
}

func TestIdentityOperationPreviewBindsCandidateEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchParticipant, LeftID: a, RightKind: IdentityMatchParticipant, RightID: b,
		Basis: IdentityMatchEmail, NormalizedValue: new("candidate-evidence@example.test"), State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
	})
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	before, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, IdentityMatchEvidenceInput{EvidenceKind: "verified_identity", Detail: new("Synthetic new evidence"), Source: ProvenanceArchiveObservation})
	requirements.NoError(err)
	after, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Equal(before.IdentityRevision, after.IdentityRevision)
	assertions.NotEqual(before.Fingerprint, after.Fingerprint, "candidate provenance includes its native evidence rows")
}
