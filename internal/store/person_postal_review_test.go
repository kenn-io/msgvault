package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/postaladdress"
)

const postalClaimJSON = `{"street_address":" 12 Example Street ","extended_address":" Unit 7 ","locality":"Example City","country_code":" ca ","extended_components":";;12 Example Street;Example City;;;;Room\\;3;Floor 2","source_text":"  My address is 12 Example Street, Unit 7.  "}`

func TestPublicPostalAddressDistinguishesLegacyTextFromSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	encoded, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal", OriginalValue: "Old street"})
	require.NoError(err)
	snapshot, err := (PersonAddress{AddressKind: PersonAddressPostal, OriginalValue: encoded, CountryCode: new("CA")}).PublicAddress()
	require.NoError(err)
	assert.Nil(snapshot.FreeText)
}

func postalGeneration(t *testing.T, st *Store, personID int64, suffix string, claims []personfacts.ProposedClaim, statuses []personfacts.EvidenceStatusChange) personfacts.GenerationInput {
	t.Helper()
	input := personFactProjectionInput(personID, suffix, claims, statuses)
	input.Policy.AllowSensitive = true
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	input.CatalogFingerprint = catalog.Fingerprint
	return input
}

func TestPostalAddressSweepExtractionRetainsWithoutProjection(t *testing.T) {
	f := newPersonSweepApplyFixtureWithBudgetAndSensitive(t, "postal", true, personSweepApplyBudget(), true)
	target := projectionTargetBySlug(t, f.store, "postal_address")
	source, err := f.store.GetOrCreateSource("slack", "postal-synthetic")
	require := require.New(t)
	require.NoError(err)
	conversationID, err := f.store.EnsureConversationWithType(source.ID, "postal-synthetic", "direct_chat", "Synthetic address")
	require.NoError(err)
	participantID, err := f.store.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	body := "My address is 12 Example Street, Unit 7."
	messageID, err := f.store.PersistMessage(&MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: "postal-source", ConversationID: conversationID, MessageType: "slack_message", SenderID: sql.NullInt64{Int64: participantID, Valid: true}, SentAt: sql.NullTime{Time: personFactLedgerNow.Add(-time.Hour), Valid: true}}, BodyText: sql.NullString{String: body, Valid: true}})
	require.NoError(err)
	items, err := f.store.HydratePersonSweepMessages(t.Context(), f.personID, []int64{messageID}, 0)
	require.NoError(err)
	require.Len(items, 1)
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	packet := peoplesweep.EvidencePacket{PersonID: f.personID, ProgramID: peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion, Catalog: catalog, Seeds: items}
	batches, err := peoplesweep.PartitionEvidencePacket(packet, 256*1024, 200)
	require.NoError(err)
	require.Len(batches, 1)
	wire, err := peoplesweep.CanonicalPacketJSON(batches[0].Packet)
	require.NoError(err)
	var envelope struct {
		Seeds []struct {
			ID string `json:"id"`
		} `json:"seeds"`
	}
	require.NoError(json.Unmarshal(wire, &envelope))
	require.Len(envelope.Seeds, 1)
	output := fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":%s,"evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":990}]}`, target.Key, postalClaimJSON, envelope.Seeds[0].ID)
	claims, err := peoplesweep.ParseExtraction(json.RawMessage(output), batches[0], peoplesweep.ProviderProfile{AllowSensitive: true})
	require.NoError(err)
	f.request.Generation.Claims = claims
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, PersonSweepEvidenceAligner{Store: f.store})
	require.NoError(err)
	require.Nil(prepared.Claims()[0].Failure)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	require.NoError(err)
	rows, err := f.store.ListPersonFactClaimsContext(t.Context(), f.personID, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	require.Len(rows, 1)
	require.NotNil(rows[0].Normalized)
	assert := assert.New(t)
	assert.JSONEq(postalClaimJSON, string(rows[0].SubmittedValue))
	require.NotNil(rows[0].PostalReview)
	assert.True(rows[0].PostalReview.Eligible)
	assert.NotEmpty(rows[0].PostalReview.SupportingEvidenceIDs)
	require.NotNil(rows[0].PostalReview.Assessment)
	addresses, err := f.store.ListPersonAddressesContext(t.Context(), f.personID, false)
	require.NoError(err)
	assert.Empty(addresses)
	decisions, err := f.store.ListPersonFactDecisionsContext(t.Context(), f.personID, personfacts.DecisionFilter{})
	require.NoError(err)
	assert.Empty(decisions, "review-only claims must not invent terminal or threshold decisions")
	state, err := f.store.LoadPersonFactState(t.Context(), f.personID, catalog)
	require.NoError(err)
	assert.Len(state.Unresolved, 1)
	assert.Empty(state.Current)

	old, err := f.store.AddPersonAddressContext(t.Context(), f.personID, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("Old street"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(err)
	rows, err = f.store.ListPersonFactClaimsContext(t.Context(), f.personID, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	review := rows[0].PostalReview
	require.True(review.Eligible)
	input := review.Assessment.Proposed
	snapshot, err := postaladdress.EncodeOriginalInput(review.Assessment.Original)
	require.NoError(err)
	sourceRef := "person-fact-claim:" + rows[0].ClaimKey
	patch := PersonProfilePatch{Addresses: &PersonAddressPatch{Supersede: []int64{old.Envelope.ID}, Add: []PersonAddressInput{{AddressKind: PersonAddressPostal, StreetAddress: &input.StreetAddress, ExtendedComponents: &input.ExtendedComponents, OriginalValue: snapshot, Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &sourceRef}}}}}
	current, err := f.store.ApplyPersonProfilePatchContext(t.Context(), f.personID, review.PersonRevision, patch)
	require.NoError(err)
	require.Len(current.Addresses, 1)
	assert.Equal(&sourceRef, current.Addresses[0].Envelope.SourceRef)
	history, err := f.store.GetPersonProfileHistoryContext(t.Context(), f.personID)
	require.NoError(err)
	require.Len(history.Addresses, 2)
	assert.NotNil(history.Addresses[0].Envelope.SupersededAt)
	assert.Equal(review.Assessment.Original, current.Addresses[0].Assessment.Original)
	rows, err = f.store.ListPersonFactClaimsContext(t.Context(), f.personID, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	assert.False(rows[0].PostalReview.Eligible)
	assert.Contains(rows[0].PostalReview.ReasonCodes, "already_adopted")
}

func TestPostalAddressReviewUsesCompleteCurrentSupport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*personfacts.ProposedClaim)
		want   string
	}{
		{"supported", func(*personfacts.ProposedClaim) {}, ""},
		{"durable failure", func(c *personfacts.ProposedClaim) {
			c.SubmittedValue = json.RawMessage(`{"street_address":"missing source"}`)
		}, "malformed-value"},
		{"identity", func(c *personfacts.ProposedClaim) { c.Evidence[0].IdentityScore = 1 }, "identity-mismatch"},
		{"future", func(c *personfacts.ProposedClaim) { c.ValidFrom = new(time.Now().UTC().Add(time.Hour)) }, "outside-validity"},
		{"expired", func(c *personfacts.ProposedClaim) { c.ValidUntil = new(personFactLedgerNow.Add(-time.Hour)) }, "outside-validity"},
		{"contradict", func(c *personfacts.ProposedClaim) { c.Relation = personfacts.RelationContradict }, "relation_not_support"},
		{"stale", func(c *personfacts.ProposedClaim) { c.Target.Description = "changed descriptor" }, "stale-target-revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, id, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "postal_address")
			claim := personFactProjectionClaim(id, target, postalClaimJSON, tc.name)
			tc.change(&claim)
			_, err := st.ApplyPersonFactGenerationContext(t.Context(), postalGeneration(t, st, id, tc.name, []personfacts.ProposedClaim{claim}, nil), nil)
			require := require.New(t)
			require.NoError(err)
			rows, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 1})
			require.NoError(err)
			require.Len(rows, 1)
			review := rows[0].PostalReview
			require.NotNil(review)
			assert := assert.New(t)
			assert.Equal(tc.want == "", review.Eligible)
			if tc.want != "" {
				assert.Contains(review.ReasonCodes, tc.want)
			}
			if tc.name == "supported" {
				profile, err := st.GetPersonProfileContext(t.Context(), id)
				require.NoError(err)
				assert.Equal(profile.Person.Revision, review.PersonRevision)
			}
		})
	}
	st, id, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "postal_address")
	claim := personFactProjectionClaim(id, target, postalClaimJSON, "support-change")
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), postalGeneration(t, st, id, "initial", []personfacts.ProposedClaim{claim}, nil), nil)
	require := require.New(t)
	require.NoError(err)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), id, personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(evidence, 1)
	for index, supported := range []bool{false, true} {
		reason := personfacts.EvidenceStatusSourceDeleted
		if supported {
			reason = personfacts.EvidenceStatusSourceReimported
		}
		input := postalGeneration(t, st, id, fmt.Sprintf("support-%d", index), nil, []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion, Supported: supported, Reason: reason}})
		input.ResolvedAt = time.Now().UTC().Add(time.Duration(index) * time.Minute)
		_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
		require.NoError(err)
		rows, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 1})
		require.NoError(err)
		require.Len(rows, 1)
		assert.Equal(t, supported, rows[0].PostalReview.Eligible)
	}
}

func TestPostalAddressPacketBoundsBacklogAndExcludesAdoptedHistory(t *testing.T) {
	st, id, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "postal_address")
	claims := make([]personfacts.ProposedClaim, 1005)
	for i := range claims {
		claims[i] = personFactProjectionClaim(id, target, postalClaimJSON, fmt.Sprintf("backlog-%04d", i))
	}
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), postalGeneration(t, st, id, "backlog", claims, nil), nil)
	require := require.New(t)
	require.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	state, err := st.LoadPersonFactState(t.Context(), id, catalog)
	require.NoError(err)
	require.Len(state.Unresolved, 100)
	selected := map[string]bool{}
	for _, claim := range state.Unresolved {
		selected[claim.ClaimKey] = true
	}
	assert := assert.New(t)
	selectedKey := state.Unresolved[0].ClaimKey
	ref := "person-fact-claim:" + selectedKey
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressBirthPlace, Locality: new("Example City"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
	require.NoError(err)
	state, err = st.LoadPersonFactState(t.Context(), id, catalog)
	require.NoError(err)
	selected = map[string]bool{}
	for _, claim := range state.Unresolved {
		selected[claim.ClaimKey] = true
	}
	assert.True(selected[selectedKey])
	address, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
	require.NoError(err)
	require.NoError(st.SupersedePersonAddressContext(t.Context(), id, address.Envelope.ID, nil))
	state, err = st.LoadPersonFactState(t.Context(), id, catalog)
	require.NoError(err)
	require.Len(state.Unresolved, 100)
	for _, claim := range state.Unresolved {
		assert.NotEqual(selectedKey, claim.ClaimKey)
	}
}

func TestPostalAddressCurrentStateExcludesBirthPlace(t *testing.T) {
	st, id, _ := newPersonFactProjectionStore(t)
	_, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), ExtendedAddress: new("Unit 7"), CountryCode: new("CA"), OriginalValue: "raw original", Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require := require.New(t)
	require.NoError(err)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressBirthPlace, Locality: new("Example City"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	state, err := st.LoadPersonFactState(t.Context(), id, catalog)
	require.NoError(err)
	require.Len(state.Current, 1)
	assert := assert.New(t)
	assert.Equal("system:postal_address", state.Current[0].TargetKey)
	var value personfacts.PostalAddressValue
	require.NoError(json.Unmarshal(state.Current[0].Value, &value))
	assert.Equal("12 Example Street", value.StreetAddress)
	assert.Equal("Unit 7", value.ExtendedAddress)
	assert.Equal("raw original", value.SourceText)
}

func TestPostalAddressRejectsUnsupportedSnapshots(t *testing.T) {
	st, id, _ := newPersonFactProjectionStore(t)
	empty, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal"})
	require.NoError(t, err)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, OriginalValue: empty, Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.ErrorIs(t, err, ErrPersonAddressValueMissing)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, OriginalValue: "msgvault:postal-input:v2\n{}", StreetAddress: new("12 Example Street"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.Error(t, err)
	_, err = (PersonAddress{AddressKind: PersonAddressPostal, OriginalValue: "msgvault:postal-input:v2\n{}"}).PublicAddress()
	require.Error(t, err)
	address, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, OriginalValue: "12 Example Street", Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`UPDATE person_addresses SET original_value = ? WHERE id = ?`), "msgvault:postal-input:v2\n{}", address.Envelope.ID)
	require.NoError(t, err)
	_, err = st.ListPersonAddressesContext(t.Context(), id, false)
	require.Error(t, err)
}
