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

func TestLegacyPostalAddressShape(t *testing.T) {
	for _, tc := range []struct {
		name, original       string
		structured, extended bool
	}{
		{"short prefix", ";;12 Example St", true, false},
		{"short box", "Box 1;;12 Example St;City", true, false},
		{"seven parts", "Box 1;Unit 7;12 Example St;City;;;", true, false},
		{"extended parts", `;;12 Example St;City;;;;Room\;3;Floor 2`, true, true},
		{"geo text", "geo:1,2;u=35", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			address, err := (PersonAddress{AddressKind: PersonAddressPostal, OriginalValue: tc.original}).PublicAddress()
			require.NoError(err)
			if tc.structured {
				require.NotNil(address.StreetAddress)
				assert.Equal("12 Example St", *address.StreetAddress)
				assert.Nil(address.FreeText)
				if tc.extended {
					require.NotNil(address.ExtendedComponents)
					assert.Equal(tc.original, *address.ExtendedComponents)
				}
			} else {
				require.NotNil(address.FreeText)
				assert.Equal(tc.original, *address.FreeText)
				assert.Nil(address.PostOfficeBox)
			}
		})
	}
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

func TestPostalAddressSweepExtractionAndAdoption(t *testing.T) {
	f := newPersonSweepApplyFixtureWithBudgetAndSensitive(t, "postal", true, personSweepApplyBudget(), true)
	target := projectionTargetBySlug(t, f.store, "postal_address")
	source, err := f.store.GetOrCreateSource("slack", "postal-synthetic")
	rootRequire := require.New(t)
	rootRequire.NoError(err)
	conversationID, err := f.store.EnsureConversationWithType(source.ID, "postal-synthetic", "direct_chat", "Synthetic address")
	rootRequire.NoError(err)
	participantID, err := f.store.EnsureParticipant("alice@example.com", "Alice", "example.com")
	rootRequire.NoError(err)
	body := "My address is 12 Example Street, Unit 7."
	messageID, err := f.store.PersistMessage(&MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: "postal-source", ConversationID: conversationID, MessageType: "slack_message", SenderID: sql.NullInt64{Int64: participantID, Valid: true}, SentAt: sql.NullTime{Time: personFactLedgerNow.Add(-time.Hour), Valid: true}}, BodyText: sql.NullString{String: body, Valid: true}})
	rootRequire.NoError(err)
	items, err := f.store.HydratePersonSweepMessages(t.Context(), f.personID, []int64{messageID}, 0)
	rootRequire.NoError(err)
	rootRequire.Len(items, 1)
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), true)
	rootRequire.NoError(err)
	packet := peoplesweep.EvidencePacket{PersonID: f.personID, ProgramID: peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion, Catalog: catalog, Seeds: items}
	batches, err := peoplesweep.PartitionEvidencePacket(packet, 256*1024, 200)
	rootRequire.NoError(err)
	rootRequire.Len(batches, 1)
	wire, err := peoplesweep.CanonicalPacketJSON(batches[0].Packet)
	rootRequire.NoError(err)
	var envelope struct {
		Seeds []struct {
			ID string `json:"id"`
		} `json:"seeds"`
	}
	rootRequire.NoError(json.Unmarshal(wire, &envelope))
	rootRequire.Len(envelope.Seeds, 1)
	output := fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":%s,"evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":990}]}`, target.Key, postalClaimJSON, envelope.Seeds[0].ID)
	claims, err := peoplesweep.ParseExtraction(json.RawMessage(output), batches[0], peoplesweep.ProviderProfile{AllowSensitive: true})
	rootRequire.NoError(err)
	f.request.Generation.Claims = claims
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, PersonSweepEvidenceAligner{Store: f.store})
	rootRequire.NoError(err)
	rootRequire.Nil(prepared.Claims()[0].Failure)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	rootRequire.NoError(err)
	rows, err := f.store.ListPersonFactClaimsContext(t.Context(), f.personID, personfacts.ClaimFilter{Limit: 1})
	rootRequire.NoError(err)
	rootRequire.Len(rows, 1)
	rootRequire.NotNil(rows[0].Normalized)
	rootAssert := assert.New(t)
	rootAssert.JSONEq(postalClaimJSON, string(rows[0].SubmittedValue))
	rootRequire.NotNil(rows[0].PostalReview)
	rootAssert.NotEmpty(rows[0].PostalReview.SupportingEvidenceIDs)
	rootRequire.NotNil(rows[0].PostalReview.Assessment)
	t.Run("retains without projection", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
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
	})

	t.Run("adoption and edit", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		old, err := f.store.AddPersonAddressContext(t.Context(), f.personID, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("Old street"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
		require.NoError(err)
		rows, err := f.store.ListPersonFactClaimsContext(t.Context(), f.personID, personfacts.ClaimFilter{Limit: 1})
		require.NoError(err)
		review := rows[0].PostalReview
		require.True(review.Eligible)
		input := review.Assessment.Proposed
		input.StreetAddress = "Corrected street"
		snapshot, err := postaladdress.EncodeOriginalInput(review.Assessment.Original)
		require.NoError(err)
		sourceRef := "person-fact-claim:" + rows[0].ClaimKey
		patch := PersonProfilePatch{Addresses: &PersonAddressPatch{Supersede: []int64{old.Envelope.ID}, Add: []PersonAddressInput{{AddressKind: PersonAddressPostal, StreetAddress: &input.StreetAddress, ExtendedComponents: &input.ExtendedComponents, OriginalValue: snapshot, Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &sourceRef}}}}}
		current, err := f.store.ApplyPersonProfilePatchContext(t.Context(), f.personID, review.PersonRevision, patch)
		require.NoError(err)
		require.Len(current.Addresses, 1)
		assert.Equal(&sourceRef, current.Addresses[0].Envelope.SourceRef)
		assert.Equal("Corrected street", *current.Addresses[0].StreetAddress)
		patch.Addresses.Supersede = []int64{current.Addresses[0].Envelope.ID}
		patch.Addresses.Add[0].StreetAddress = new("New street")
		current, err = f.store.ApplyPersonProfilePatchContext(t.Context(), f.personID, current.Person.Revision, patch)
		require.NoError(err)
		require.Len(current.Addresses, 1)
		assert.Equal("New street", *current.Addresses[0].StreetAddress)
		assert.Equal(&sourceRef, current.Addresses[0].Envelope.SourceRef)
		rows, err = f.store.ListPersonFactClaimsContext(t.Context(), f.personID, personfacts.ClaimFilter{Limit: 1})
		require.NoError(err)
		assert.False(rows[0].PostalReview.Eligible)
		assert.Contains(rows[0].PostalReview.ReasonCodes, "already_adopted")
		// A merge or subset copy may keep the address without its source claim.
		_, err = f.store.db.ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_fact_claims WHERE person_id = ?`), f.personID)
		require.NoError(err)
		patch.Addresses.Supersede = []int64{current.Addresses[0].Envelope.ID}
		patch.Addresses.Add[0].StreetAddress = new("Later street")
		current, err = f.store.ApplyPersonProfilePatchContext(t.Context(), f.personID, current.Person.Revision, patch)
		require.NoError(err)
		require.Len(current.Addresses, 1)
		assert.Equal("Later street", *current.Addresses[0].StreetAddress)
		assert.Equal(&sourceRef, current.Addresses[0].Envelope.SourceRef)
		other, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal", StreetAddress: "Different source"})
		require.NoError(err)
		patch.Addresses.Supersede = []int64{current.Addresses[0].Envelope.ID}
		patch.Addresses.Add[0].OriginalValue = other
		_, err = f.store.ApplyPersonProfilePatchContext(t.Context(), f.personID, current.Person.Revision, patch)
		require.ErrorIs(err, ErrPostalSuggestionMismatch)
	})
}

func TestPostalSuggestionsMatchOnlyCurrentAddressComponents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "postal_address")
	_, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new(" 12 Example Street "), ExtendedAddress: new(" Unit 7 "), CountryCode: new(" ca "), Label: new("home"), OriginalValue: "Entered by hand", Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(err)
	old, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("Old Street"), CountryCode: new("CA"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(err)
	require.NoError(st.SupersedePersonAddressContext(t.Context(), id, old.Envelope.ID, nil))
	original, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal", StreetAddress: "Original draft", CountryCode: "CA"})
	require.NoError(err)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("Current street"), CountryCode: new("CA"), OriginalValue: original, Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(err)
	for _, tc := range []struct{ suffix, value string }{
		{"first", `{"street_address":"12 Example Street","extended_address":"Unit 7","country_code":"CA","source_text":"first evidence"}`},
		{"second", `{"street_address":"12 Example Street","extended_address":"Unit 7","country_code":"CA","source_text":"second evidence"}`},
		{"different unit", `{"street_address":"12 Example Street","extended_address":"Unit 8","country_code":"CA","source_text":"other unit"}`},
		{"history", `{"street_address":"Old Street","country_code":"CA","source_text":"former home"}`},
		{"different extended components", `{"street_address":"12 Example Street","extended_address":"Unit 7","country_code":"CA","extended_components":";;;;;;;Floor 2","source_text":"other floor"}`},
		{"original draft", `{"street_address":"Original draft","country_code":"CA","source_text":"earlier draft"}`},
	} {
		claim := personFactProjectionClaim(id, target, tc.value, tc.suffix)
		_, err := st.ApplyPersonFactGenerationContext(t.Context(), postalGeneration(t, st, id, tc.suffix, []personfacts.ProposedClaim{claim}, nil), nil)
		require.NoError(err)
	}
	claims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 10})
	require.NoError(err)
	require.Len(claims, 6)
	wantEligible := map[string]bool{"first evidence": false, "second evidence": false, "other unit": true, "former home": true, "other floor": true, "earlier draft": true}
	for _, claim := range claims {
		require.NotNil(claim.PostalReview)
		require.NotNil(claim.PostalReview.Assessment)
		source := claim.PostalReview.Assessment.Original.OriginalValue
		eligible, ok := wantEligible[source]
		require.True(ok, "unexpected source %q", source)
		assert.Equal(eligible, claim.PostalReview.Eligible, source)
		if !eligible {
			assert.Contains(claim.PostalReview.ReasonCodes, "already_adopted")
			snapshot, err := postaladdress.EncodeOriginalInput(claim.PostalReview.Assessment.Original)
			require.NoError(err)
			ref := "person-fact-claim:" + claim.ClaimKey
			_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), OriginalValue: snapshot, Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
			assert.ErrorIs(err, ErrPostalSuggestionInvalid)
		}
	}
}

func TestPostalClaimReferenceCannotHideDifferentSuggestion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "postal_address")
	claim := personFactProjectionClaim(id, target, postalClaimJSON, "different-address")
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), postalGeneration(t, st, id, "different-address", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	require.Len(claims, 1)
	require.True(claims[0].PostalReview.Eligible)
	ref := "person-fact-claim:" + claims[0].ClaimKey
	original, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal", StreetAddress: "99 Other Road", OriginalValue: "99 Other Road"})
	require.NoError(err)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("99 Other Road"), OriginalValue: original, Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
	require.ErrorIs(err, ErrPostalSuggestionMismatch, "a different address must not consume this suggestion")
	claims, err = st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	require.Len(claims, 1)
	assert.True(claims[0].PostalReview.Eligible, "a different address must not consume this suggestion")
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("99 Other Road"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
	require.ErrorIs(err, ErrPostalSuggestionMismatch, "a different address without a snapshot must not consume this suggestion")
	claims, err = st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	require.Len(claims, 1)
	assert.True(claims[0].PostalReview.Eligible, "a different address without a snapshot must not consume this suggestion")
}

func TestPostalClaimReferenceRequiresMatchingPostalClaim(t *testing.T) {
	require := require.New(t)
	st, id, targets := newPersonFactProjectionStore(t)
	claim := personFactProjectionClaim(id, targets[AttributeSlugPrimaryChannel], `"chat"`, "nonpostal")
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), postalGeneration(t, st, id, "nonpostal", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{Limit: 1})
	require.NoError(err)
	require.Len(claims, 1)
	for _, key := range []string{"missing", claims[0].ClaimKey} {
		ref := "person-fact-claim:" + key
		_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
		require.ErrorIs(err, ErrPostalSuggestionInvalid)
	}
}

func TestPostalAddressReviewUsesCompleteCurrentSupport(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		change                           func(*personfacts.ProposedClaim)
		want                             string
		missingAssessment, checkRevision bool
	}{
		{"supported", func(*personfacts.ProposedClaim) {}, "", false, true},
		{"durable failure", func(c *personfacts.ProposedClaim) {
			c.SubmittedValue = json.RawMessage(`{"street_address":"missing source"}`)
		}, "malformed-value", false, false},
		{"unknown field", func(c *personfacts.ProposedClaim) {
			c.SubmittedValue = json.RawMessage(`{"street_address":"12 Example St","source_text":"12 Example St","apartment":"7"}`)
		}, "malformed-value", true, false},
		{"identity", func(c *personfacts.ProposedClaim) { c.Evidence[0].IdentityScore = 1 }, "identity-mismatch", false, false},
		{"future", func(c *personfacts.ProposedClaim) { c.ValidFrom = new(time.Now().UTC().Add(time.Hour)) }, "outside-validity", false, false},
		{"expired", func(c *personfacts.ProposedClaim) { c.ValidUntil = new(personFactLedgerNow.Add(-time.Hour)) }, "outside-validity", false, false},
		{"contradict", func(c *personfacts.ProposedClaim) { c.Relation = personfacts.RelationContradict }, "relation_not_support", false, false},
		{"stale", func(c *personfacts.ProposedClaim) { c.Target.Description = "changed descriptor" }, "stale-target-revision", false, false},
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
			if tc.missingAssessment {
				assert.Nil(review.Assessment)
			}
			if tc.checkRevision {
				profile, err := st.GetPersonProfileContext(t.Context(), id)
				require.NoError(err)
				assert.Equal(profile.Person.Revision, review.PersonRevision)
			}
		})
	}
}

func TestPostalAddressReviewTracksEvidenceSupport(t *testing.T) {
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
		if !supported {
			original, err := postaladdress.EncodeOriginalInput(rows[0].PostalReview.Assessment.Original)
			require.NoError(err)
			ref := "person-fact-claim:" + rows[0].ClaimKey
			_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), OriginalValue: original, Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
			require.ErrorIs(err, ErrPostalSuggestionInvalid)
		}
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
	rootRequire := require.New(t)
	rootRequire.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	rootRequire.NoError(err)
	state, err := st.LoadPersonFactState(t.Context(), id, catalog)
	rootRequire.NoError(err)
	rootRequire.Len(state.Unresolved, 100)
	t.Run("budget", func(t *testing.T) {
		require := require.New(t)
		for _, remaining := range []int{0, 1, 99, 100} {
			err = st.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
				claims, err := st.loadPersonSweepUnresolvedClaimsTx(t.Context(), tx, id, target, remaining)
				if err == nil {
					assert.Len(t, claims, remaining)
				}
				return err
			})
			require.NoError(err, "remaining budget %d", remaining)
		}
	})

	t.Run("adopted history", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		selectedKey := state.Unresolved[0].ClaimKey
		ref := "person-fact-claim:" + selectedKey
		_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressBirthPlace, Locality: new("Example City"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
		require.NoError(err)
		state, err = st.LoadPersonFactState(t.Context(), id, catalog)
		require.NoError(err)
		selected := map[string]bool{}
		for _, claim := range state.Unresolved {
			selected[claim.ClaimKey] = true
		}
		assert.True(selected[selectedKey])
		var submitted personfacts.PostalAddressValue
		require.NoError(json.Unmarshal(state.Unresolved[0].SubmittedValue, &submitted))
		original, err := postaladdress.EncodeOriginalInput(submitted.Input())
		require.NoError(err)
		address, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), OriginalValue: original, Envelope: ValueEnvelopeInput{Source: ProvenanceUser, SourceRef: &ref}})
		require.NoError(err)
		require.NoError(st.SupersedePersonAddressContext(t.Context(), id, address.Envelope.ID, nil))
		state, err = st.LoadPersonFactState(t.Context(), id, catalog)
		require.NoError(err)
		require.Len(state.Unresolved, 100)
		for _, claim := range state.Unresolved {
			assert.NotEqual(selectedKey, claim.ClaimKey)
		}
	})
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

func TestPostalAddressSnapshotReadAndWriteBoundaries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	encoded, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal", OriginalValue: "Old street"})
	require.NoError(err)
	snapshot, err := (PersonAddress{AddressKind: PersonAddressPostal, OriginalValue: encoded, CountryCode: new("CA")}).PublicAddress()
	require.NoError(err)
	assert.Nil(snapshot.FreeText)
	empty, err := postaladdress.EncodeOriginalInput(postaladdress.Input{AddressKind: "postal"})
	require.NoError(err)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, OriginalValue: empty, Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.ErrorIs(err, ErrPersonAddressValueMissing)
	_, err = st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, OriginalValue: "msgvault:postal-input:v2\n{}", StreetAddress: new("12 Example Street"), Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.ErrorIs(err, postaladdress.ErrUnsupportedEnvelope)
	public, err := (PersonAddress{AddressKind: PersonAddressPostal, StreetAddress: new("12 Example Street"), OriginalValue: "msgvault:postal-input:v2\n{}"}).PublicAddress()
	require.NoError(err)
	assert.Equal("12 Example Street", *public.StreetAddress)
	assert.Empty(public.OriginalValue)
	address, err := st.AddPersonAddressContext(t.Context(), id, PersonAddressInput{AddressKind: PersonAddressPostal, OriginalValue: "12 Example Street", Envelope: ValueEnvelopeInput{Source: ProvenanceUser}})
	require.NoError(err)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`UPDATE person_addresses SET original_value = ? WHERE id = ?`), "msgvault:postal-input:v2\n{}", address.Envelope.ID)
	require.NoError(err)
	listed, err := st.ListPersonAddressesContext(t.Context(), id, false)
	require.NoError(err)
	require.Len(listed, 1)
}
