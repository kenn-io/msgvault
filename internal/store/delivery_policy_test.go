package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDeliveryPolicyAuditLookupUsesIndexSQLite(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	for _, name := range []string{"fresh", "upgrade"} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			if name == "upgrade" {
				_, err := st.DB().ExecContext(t.Context(), `DROP INDEX idx_delivery_policy_audit_latest`)
				require.NoError(err)
				require.NoError(st.InitSchemaContext(t.Context()))
			}
			rows, err := st.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN
				SELECT created_at FROM delivery_policy_audit WHERE person_uid=? AND scope=? ORDER BY revision DESC LIMIT 1`,
				"example-person", store.DeliveryPersonAllRoutes)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(rows.Err())
			plan := strings.Join(details, "\n")
			assert.Contains(plan, "SEARCH delivery_policy_audit USING INDEX idx_delivery_policy_audit_latest")
			assert.NotContains(plan, "USE TEMP B-TREE")
		})
	}
}

func deliveryContactPoint(t *testing.T, f *storetest.Fixture, personID int64, address string) {
	t.Helper()
	_, err := f.Store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: address,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
}

// deliveryPerson returns a person with two saved addresses.
func deliveryPerson(t *testing.T, f *storetest.Fixture) (*store.Person, []string) {
	t.Helper()
	p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("peer@example.test", "Example Person", "example.test"))
	require.NoError(t, err)
	addresses := []string{"first@example.test", "second@example.test"}
	for _, address := range addresses {
		deliveryContactPoint(t, f, p.ID, address)
	}
	return p, addresses
}

func readDelivery(t *testing.T, st *store.Store, uid, endpoint string) *store.DeliveryPolicyState {
	t.Helper()
	state, err := st.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: uid, Endpoint: endpoint})
	require.NoError(t, err)
	return state
}

func deliveryWrite(state *store.DeliveryPolicyState, policy store.DeliveryPolicy) store.DeliveryPolicyWrite {
	return store.DeliveryPolicyWrite{
		PersonUID: state.PersonUID, Endpoint: state.Endpoint,
		ExpectedRevision: state.Revision, Policy: policy,
		ScopeAcknowledgement: "person_all_routes", Actor: "owner", Reason: "Explicit synthetic approval",
	}
}

func setDelivery(t *testing.T, st *store.Store, uid, endpoint string, policy store.DeliveryPolicy) *store.DeliveryPolicyReceipt {
	t.Helper()
	receipt, err := st.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, st, uid, endpoint), policy))
	require.NoError(t, err)
	return receipt
}

func deliveryRecipient(p *store.Person, endpoint string) []store.DeliveryPolicyQuery {
	return []store.DeliveryPolicyQuery{{PersonUID: p.VCardUID, Endpoint: endpoint}}
}

func TestDeliveryPolicyDefaultsToDraftOnlyAndApprovesOneAddress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)

	initial := readDelivery(t, f.Store, p.VCardUID, addresses[0])
	assert.Equal(store.DeliveryDraftOnly, initial.EffectivePolicy)
	assert.Nil(initial.StoredPolicy)

	receipt := setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed)
	assert.Equal(store.DeliverySendAllowed, receipt.After.EffectivePolicy)
	assert.Equal(initial.Revision+1, receipt.After.Revision, "a policy change bumps the delivery revision")
	require.NotNil(receipt.Audit)
	assert.False(receipt.Audit.CreatedAt.IsZero())
	assert.Nil(receipt.Audit.BeforePolicy)

	assert.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, p.VCardUID, addresses[1]).EffectivePolicy)
}

func TestDeliveryPolicyPersonDefaultAndOverrides(t *testing.T) {
	t.Run("future point saved before active point", func(t *testing.T) {
		f := storetest.New(t)
		p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("scheduled@example.test", "Scheduled Person", "example.test"))
		require.NoError(t, err)
		synctest.Test(t, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			future := time.Now().Add(time.Hour).In(time.FixedZone("negative", -12*60*60))
			_, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "future@example.test",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser, ActiveFrom: &future},
			})
			require.NoError(err)
			duplicateFrom := time.Now().Add(30 * time.Minute)
			_, err = f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "active@example.test",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser, ActiveFrom: &duplicateFrom},
			})
			require.NoError(err)
			deliveryContactPoint(t, f, p.ID, "active@example.test")
			setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
			assert.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, "active@example.test").EffectivePolicy)
			assert.Equal("detached_endpoint", readDelivery(t, f.Store, p.VCardUID, "future@example.test").Reason)
			synctest.Sleep(45 * time.Minute)
			active := readDelivery(t, f.Store, p.VCardUID, "active@example.test")
			assert.Equal(store.DeliverySendAllowed, active.EffectivePolicy)
			assert.Equal("explicit_person_default", active.Reason)
			synctest.Sleep(75 * time.Minute)
			state := readDelivery(t, f.Store, p.VCardUID, "future@example.test")
			assert.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
			assert.Equal("approval_predates_address", state.Reason)
			reapproved := setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
			require.NotNil(reapproved.Audit)
			assert.Equal(state.Revision+1, reapproved.After.Revision)
			assert.True(reapproved.Audit.CreatedAt.After(future))
			assert.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, "future@example.test").EffectivePolicy)
			assert.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, "active@example.test").EffectivePolicy)
		})
	})

	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)

	unacknowledged := deliveryWrite(readDelivery(t, f.Store, p.VCardUID, ""), store.DeliverySendAllowed)
	unacknowledged.ScopeAcknowledgement = ""
	_, err := f.Store.SetDeliveryPolicyContext(t.Context(), unacknowledged)
	require.ErrorIs(err, store.ErrDeliveryPolicyInvalid)
	assert.Equal("scope_acknowledgement_missing", store.DeliveryErrorCode(err))
	restricted := deliveryWrite(readDelivery(t, f.Store, p.VCardUID, ""), store.DeliveryDraftOnly)
	restricted.ScopeAcknowledgement = ""
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), restricted)
	require.NoError(err, "restricting needs no acknowledgement")

	setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	inherited := readDelivery(t, f.Store, p.VCardUID, addresses[1])
	assert.Equal(store.DeliverySendAllowed, inherited.EffectivePolicy)
	assert.Equal("person_default", inherited.InheritanceSource)
	assert.Equal("explicit_person_default", inherited.Reason)

	later := []string{"later-user@example.test", "later-carddav@example.test"}
	for i, source := range []store.Provenance{store.ProvenanceUser, store.ProvenanceCardDAVImport} {
		_, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: later[i],
			Envelope: store.ValueEnvelopeInput{Source: source},
		})
		require.NoError(err)
	}
	for _, address := range later {
		state := readDelivery(t, f.Store, p.VCardUID, address)
		assert.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
		assert.Equal("approval_predates_address", state.Reason)
	}
	setDelivery(t, f.Store, p.VCardUID, later[0], store.DeliverySendAllowed)
	_, err = f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, later[0]), store.DeliveryDraftOnly))
	require.NoError(err)
	assert.Equal("approval_predates_address", readDelivery(t, f.Store, p.VCardUID, later[0]).Reason)
	setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	for _, address := range append(addresses, later...) {
		assert.Equal("explicit_person_default", readDelivery(t, f.Store, p.VCardUID, address).Reason)
	}
	inherited = readDelivery(t, f.Store, p.VCardUID, addresses[1])

	denied, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(inherited, store.DeliveryDraftOnly))
	require.NoError(err)

	cleared, err := f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(&denied.After, store.DeliveryDraftOnly))
	require.NoError(err)
	assert.Nil(cleared.After.StoredPolicy)
	assert.Equal(store.DeliverySendAllowed, cleared.After.EffectivePolicy)
	require.NotNil(cleared.Audit)
	assert.Equal(store.DeliveryDraftOnly, *cleared.Audit.BeforePolicy)
}

func TestDeliveryPolicyUnchangedWritesKeepRevisionAndAudit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	allowed := setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed).After

	repeat, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed, store.DeliverySendAllowed))
	require.NoError(err)
	assert.Nil(repeat.Audit)
	for _, endpoint := range []string{addresses[1], ""} {
		receipt, err := f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, endpoint), store.DeliveryDraftOnly))
		require.NoError(err)
		assert.Nil(receipt.Audit)
	}
	assert.Equal(allowed.Revision, readDelivery(t, f.Store, p.VCardUID, addresses[0]).Revision)
	var audits int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM delivery_policy_audit WHERE person_uid=?`), p.VCardUID).Scan(&audits))
	assert.Equal(1, audits)

	setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	_, err = f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	before := readDelivery(t, f.Store, p.VCardUID, "")
	unchanged := setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	assert.Nil(unchanged.Audit)
	assert.Equal(before.Revision, unchanged.After.Revision)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM delivery_policy_audit WHERE person_uid=?`), p.VCardUID).Scan(&audits))
	assert.Equal(2, audits)
}

func TestDeliveryPolicyRemovingAndReaddingTheAddress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	audits := func() int {
		var n int
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM delivery_policy_audit WHERE person_uid=?`), p.VCardUID).Scan(&n))
		return n
	}
	resave := func(address string, readd bool) {
		points, err := f.Store.FindPersonContactPointsContext(t.Context(), store.ContactPointQuery{AddressKind: store.ContactAddressEmail, NormalizedValue: address})
		require.NoError(err)
		require.Len(points, 1)
		require.NoError(f.Store.SupersedePersonContactPointContext(t.Context(), p.ID, points[0].Envelope.ID, nil))
		if readd {
			deliveryContactPoint(t, f, p.ID, address)
		}
	}

	// A restriction survives, even under a send_allowed default.
	setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliveryDraftOnly)
	resave(addresses[0], true)
	state := readDelivery(t, f.Store, p.VCardUID, addresses[0])
	assert.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assert.Equal("contact_method", state.InheritanceSource)
	assert.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, addresses[1]).EffectivePolicy)

	// An approval lapses, with or without a person default, and re-approving rebinds it.
	third := "third@example.test"
	deliveryContactPoint(t, f, p.ID, third)
	for _, address := range []string{addresses[1], third} {
		if address == third {
			_, err := f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, ""), store.DeliveryDraftOnly))
			require.NoError(err)
		}
		approved := setDelivery(t, f.Store, p.VCardUID, address, store.DeliverySendAllowed)
		current, err := f.Store.GetPersonContext(t.Context(), p.ID)
		require.NoError(err)
		name := "Updated Example Person " + address
		_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), p.ID, current.Revision, &name)
		require.NoError(err)
		unchanged := readDelivery(t, f.Store, p.VCardUID, address)
		assert.Greater(unchanged.Revision, approved.After.Revision)
		assert.Equal(store.DeliverySendAllowed, unchanged.EffectivePolicy)
		assert.Equal("explicit_approval", unchanged.Reason)
		resave(address, true)
		lapsed := readDelivery(t, f.Store, p.VCardUID, address)
		assert.Equal(store.DeliveryDraftOnly, lapsed.EffectivePolicy)
		assert.Equal("binding_changed", lapsed.Reason)

		before := audits()
		receipt := setDelivery(t, f.Store, p.VCardUID, address, store.DeliverySendAllowed)
		require.NotNil(receipt.Audit, "re-approving a lapsed binding is a change")
		assert.NotEqual(*approved.Audit.ContactPointID, *receipt.Audit.ContactPointID, "the new saved row is bound")
		assert.Equal(store.DeliverySendAllowed, receipt.After.EffectivePolicy)
		assert.Equal(before+1, audits())
	}

	// An approval can still be revoked and cleared after its address is removed.
	resave(third, false)
	removed := readDelivery(t, f.Store, p.VCardUID, third)
	assert.Equal(store.DeliverySendAllowed, *removed.StoredPolicy)
	assert.Equal("detached_endpoint", removed.Reason)
	denied := setDelivery(t, f.Store, p.VCardUID, third, store.DeliveryDraftOnly).After
	cleared, err := f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(&denied, store.DeliveryDraftOnly))
	require.NoError(err)
	assert.Nil(cleared.After.StoredPolicy)
}

func TestDeliveryPolicyAddressRule(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, _ := deliveryPerson(t, f)
	deliveryContactPoint(t, f, p.ID, "PEER@localhost")
	const literal = "peer@[IPv6:2001:db8::1]"
	deliveryContactPoint(t, f, p.ID, literal)

	// The address is the value the contact writer stores: trimmed and lower case.
	assert.Equal(store.DeliverySendAllowed, setDelivery(t, f.Store, p.VCardUID, "PEER@localhost", store.DeliverySendAllowed).After.EffectivePolicy)
	assert.Equal(store.DeliverySendAllowed, readDelivery(t, f.Store, p.VCardUID, "peer@localhost").EffectivePolicy)
	assert.Equal("peer@localhost", readDelivery(t, f.Store, p.VCardUID, "PEER@localhost").Endpoint, "the state names the saved address, not the spelling asked for")
	literalState := setDelivery(t, f.Store, p.VCardUID, literal, store.DeliverySendAllowed).After
	assert.Equal(store.DeliverySendAllowed, literalState.EffectivePolicy)
	assert.Equal("explicit_approval", literalState.Reason)
	// Only a plain single address is accepted: no display names, quotes or lists.
	for _, endpoint := range []string{"Peer <peer@localhost>", `"peer"@localhost`, "list-a@example.test,list-b@example.test"} {
		_, err := f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: p.VCardUID, Endpoint: endpoint})
		require.ErrorIs(err, store.ErrDeliveryPolicyInvalid, endpoint)
	}
}

func TestDeliveryPolicyAnotherPersonsAliasSpellingsMakeAnAddressAmbiguous(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, _ := deliveryPerson(t, f)
	other, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("other@example.test", "Other Person", "example.test"))
	require.NoError(err)
	_, _, err = f.Store.EnsureCommunicationServiceContext(t.Context(), store.CommunicationServiceInput{Slug: "case-preserving", DisplayLabel: "Case preserving", ScopePolicy: store.ScopePolicyNone, Normalization: store.NormalizationNone, NormalizationVersion: 1})
	require.NoError(err)
	for _, c := range []struct {
		name, address, alias string
		service, scoped      bool
	}{
		{"angle", "angle@example.test", "<angle@example.test>", false, false},
		{"named", "named@example.test", "Rival Person <named@example.test>", false, false},
		{"comment", "comment@example.test", "comment@example.test (Rival)", false, false},
		{"quoted", "quoted@example.test", `"quoted"@example.test`, false, false},
		{"group", "group@example.test", "group: group@example.test;", false, false},
		{"upper", "upper@example.test", "UPPER@example.test", false, false},
		{"tab", "tab@example.test", "tab@	example.test", false, false},
		{"ipv6 named", "one@[IPv6:2001:db8::1]", "Rival Person <one@[IPv6:2001:db8::1]>", false, false},
		{"ipv6 tag in the name", "two@[IPv6:2001:db8::2]", `"Device [IPv6:]" <two@[IPv6:2001:db8::2]>`, false, false},
		{"service plain", "service@example.test", "service@example.test", true, false},
		{"case preserving service", "first@example.test", "FIRST@EXAMPLE.TEST", true, false},
		{"unicode case preserving service", "é@éxample.test", "É@ÉXAMPLE.TEST", true, false},
		{"service spelled", "service-spelled@example.test", "Rival <service-spelled@example.test>", true, false},
		{"scoped service", "scoped@example.test", "scoped@example.test", true, true},
		{"scoped spelling", "scoped-spelled@example.test", "Rival <scoped-spelled@example.test>", true, true},
		{"scope only", "scope-only@example.test", "scope-only@example.test", false, true},
	} {
		deliveryContactPoint(t, f, p.ID, c.address)
		setDelivery(t, f.Store, p.VCardUID, c.address, store.DeliverySendAllowed)
		input := store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: c.alias,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}
		if c.service {
			input.ServiceSlug = new("google-chat")
			if c.name == "case preserving service" || c.name == "unicode case preserving service" {
				input.ServiceSlug = new("case-preserving")
			}
		}
		if c.scoped {
			input.ScopeKind, input.ScopeValue = new("account"), new("example.test")
		}
		_, err := f.Store.AddPersonContactPointContext(t.Context(), other.ID, input)
		require.NoError(err, c.name)
		state := readDelivery(t, f.Store, p.VCardUID, c.address)
		assert.Equal(store.DeliveryDraftOnly, state.EffectivePolicy, c.name)
		assert.Equal("ambiguous_endpoint", state.Reason, c.name)
	}
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, "angle@example.test"), store.DeliverySendAllowed))
	assert.Equal("ambiguous_endpoint", store.DeliveryErrorCode(err))
}

func TestDeliveryPolicyApprovalLapsesWhenTheAddressIsSplit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	survivor, _ := deliveryPerson(t, f)
	participant := f.EnsureParticipant("absorbed@example.test", "Absorbed Person", "example.test")
	absorbed, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	const address = "absorbed-address@example.test"
	deliveryContactPoint(t, f, absorbed.ID, address)
	survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "delivery-policy-split-binding", Actor: "owner",
	})
	require.NoError(err)

	setDelivery(t, f.Store, survivor.VCardUID, address, store.DeliverySendAllowed)
	survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	_, err = f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: survivor.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{participant},
		ExpectedSourceRevision: survivor.Revision, IdempotencyKey: "delivery-policy-split", Actor: "owner",
	})
	require.NoError(err)
	state := readDelivery(t, f.Store, survivor.VCardUID, address)
	assert.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assert.Equal("detached_endpoint", state.Reason)
}

func TestDeliveryPolicyReadRejectsUnknownOrMalformedPersonUID(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	p, _ := deliveryPerson(t, f)

	_, err := f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: "unknown-synthetic-uid"})
	require.ErrorIs(err, store.ErrDeliveryPersonUnknown)
	_, err = f.Store.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: p.VCardUID + "\x00"})
	require.ErrorIs(err, store.ErrDeliveryPolicyInvalid)
}

func TestDeliveryPolicyEndsOnPersonMergeOrDelete(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint bool
		onSurv   bool
		keeps    bool
		policy   store.DeliveryPolicy
	}{
		{name: "survivor default", onSurv: true, policy: store.DeliverySendAllowed},
		{name: "absorbed default"},
		{name: "survivor override", onSurv: true, endpoint: true, keeps: true, policy: store.DeliverySendAllowed},
		{name: "survivor draft_only default", onSurv: true, keeps: true},
		{name: "absorbed override", endpoint: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			survivor, _ := deliveryPerson(t, f)
			absorbed, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("absorbed@example.test", "Absorbed Person", "example.test"))
			require.NoError(err)
			deliveryContactPoint(t, f, absorbed.ID, "absorbed-address@example.test")
			owner := absorbed
			if test.onSurv {
				owner = survivor
			}
			endpoint := ""
			if test.endpoint {
				endpoint = "absorbed-address@example.test"
				if test.onSurv {
					endpoint = "first@example.test"
				}
			}
			policy := test.policy
			if policy == "" {
				policy = store.DeliveryDraftOnly
			}
			setDelivery(t, f.Store, owner.VCardUID, endpoint, policy)
			survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
			require.NoError(err)
			absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
			require.NoError(err)
			var absorbedDelivery int64
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT delivery_revision FROM persons WHERE id=?`), absorbed.ID).Scan(&absorbedDelivery))

			_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey: "delivery-policy-merge-" + test.name, Actor: "merger",
			})
			require.NoError(err)
			var rows, audits int
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM delivery_policies WHERE person_id IN (?, ?)`), survivor.ID, absorbed.ID).Scan(&rows))
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM delivery_policy_audit WHERE person_uid=? AND reason='person_merged' AND actor='merger' AND after_policy IS NULL`), owner.VCardUID).Scan(&audits))
			if test.keeps {
				// The survivor's exact approval stays bound to its unchanged contact point.
				assert.Equal(1, rows)
				assert.Zero(audits)
				assert.Equal(policy, readDelivery(t, f.Store, survivor.VCardUID, endpoint).EffectivePolicy)
				return
			}
			assert.Zero(rows)
			assert.Equal(1, audits, "one audit row for the ended policy")
			var audited int64
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT revision FROM delivery_policy_audit WHERE person_uid=? AND reason='person_merged'`), owner.VCardUID).Scan(&audited))
			committed := absorbed.Revision + 1 + absorbedDelivery
			if test.onSurv {
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT revision + delivery_revision FROM persons WHERE vcard_uid=?`), owner.VCardUID).Scan(&committed))
			}
			assert.Equal(committed, audited, "the audit row carries the committed revision")
			// A merge never carries an approval over to the absorbed person's address.
			assert.Equal(store.DeliveryDraftOnly, readDelivery(t, f.Store, survivor.VCardUID, "absorbed-address@example.test").EffectivePolicy)
		})
	}

	t.Run("person delete", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		p, _ := deliveryPerson(t, f)
		setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
		current, err := f.Store.GetPersonContext(t.Context(), p.ID)
		require.NoError(err)
		require.NoError(f.Store.DeletePersonContext(t.Context(), current.ID, current.Revision))
		var audits int
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM delivery_policy_audit WHERE person_uid=? AND reason='person_deleted' AND before_policy='send_allowed' AND after_policy IS NULL`), p.VCardUID).Scan(&audits))
		assert.Equal(1, audits, "the cascade drops the default, with an audit row")
	})

	t.Run("unrelated people", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		var created []*store.Person
		for _, email := range []string{"unrelated-one@example.test", "unrelated-two@example.test"} {
			person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant(email, "Unrelated Person", "example.test"))
			require.NoError(err)
			created = append(created, person)
		}
		_, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: created[0].ID, AbsorbedID: created[1].ID,
			ExpectedSurvivorRevision: created[0].Revision, ExpectedAbsorbedRevision: created[1].Revision,
			IdempotencyKey: "delivery-policy-unrelated-merge", Actor: "owner",
		})
		require.NoError(err)
		var audits int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM delivery_policy_audit`).Scan(&audits))
		assert.Zero(audits, "a merge without policies writes no audit row")
	})
}

func TestDeliveryAdmissionRunsOnlyApprovedRecipients(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	calls := 0
	send := func(context.Context) error { calls++; return nil }
	var refusal *store.DeliveryAdmissionError

	err := f.Store.WithDeliveryAdmissionContext(t.Context(), deliveryRecipient(p, addresses[0]), send)
	require.ErrorAs(err, &refusal)
	assert.Equal("draft_required", refusal.Code)
	assert.False(refusal.ProviderAttempted)
	assert.Zero(calls)

	allowed := setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed).After
	both := append(deliveryRecipient(p, addresses[0]), deliveryRecipient(p, addresses[1])...)
	both = append(both, deliveryRecipient(p, "missing@example.test")...)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), both, send)
	require.ErrorAs(err, &refusal)
	assert.Equal("draft_required", refusal.Code)
	require.Len(refusal.Policies, 3)
	for i, recipient := range both {
		assert.Equal(*readDelivery(t, f.Store, recipient.PersonUID, recipient.Endpoint), refusal.Policies[i])
	}
	assert.Equal(store.DeliverySendAllowed, refusal.Policies[0].EffectivePolicy)
	assert.Equal("draft_required", refusal.Policies[1].Reason)
	assert.Equal("draft_required", refusal.Policies[2].Reason)
	assert.False(refusal.ProviderAttempted)
	assert.Zero(calls, "one unapproved recipient blocks the whole envelope")

	require.NoError(f.Store.WithDeliveryAdmissionContext(t.Context(), deliveryRecipient(p, addresses[0]), send))
	assert.Equal(1, calls)

	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed, store.DeliveryDraftOnly))
	require.NoError(err)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), deliveryRecipient(p, addresses[0]), send)
	require.ErrorAs(err, &refusal)
	assert.Equal(1, calls, "revocation between two attempts blocks the second")

	// A send_allowed default would admit a person-wide recipient if the guard let it through.
	setDelivery(t, f.Store, p.VCardUID, "", store.DeliverySendAllowed)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), deliveryRecipient(p, ""), send)
	require.ErrorAs(err, &refusal, "a recipient needs an exact address")
	assert.Equal("unresolved_recipient", refusal.Reason)
	assert.Equal(1, calls)

	other, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("other@example.test", "Other Person", "example.test"))
	require.NoError(err)
	deliveryContactPoint(t, f, p.ID, "shared@example.test")
	deliveryContactPoint(t, f, other.ID, "Rival Person <shared@example.test>")
	batch := append(deliveryRecipient(p, addresses[1]), deliveryRecipient(p, "shared@example.test")...)
	batch = append(batch, deliveryRecipient(p, "missing@example.test")...)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), batch, send)
	require.ErrorAs(err, &refusal)
	assert.Equal("ambiguous_endpoint", refusal.Reason, "the first blocked recipient decides")
	assert.Equal(1, calls)

	// ExecuteDeliveryContext adds the recoverable content and the receipt.
	content := []byte("synthetic recoverable draft")
	receipt, err := f.Store.ExecuteDeliveryContext(t.Context(), deliveryRecipient(p, addresses[0]), content, send)
	require.Error(err)
	assert.Equal(content, receipt.Content)
	assert.False(receipt.ProviderAttempted)
	setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed)
	receipt, err = f.Store.ExecuteDeliveryContext(t.Context(), deliveryRecipient(p, addresses[0]), content, send)
	require.NoError(err)
	assert.Equal("sent", receipt.Code)
	assert.True(receipt.ProviderAttempted)
	assert.Equal(2, calls)
}

func TestDeliveryAdmissionRefusesAnInheritedSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed)
	snapshotCtx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()

	calls := 0
	err = f.Store.WithDeliveryAdmissionContext(snapshotCtx, deliveryRecipient(p, addresses[0]), func(context.Context) error { calls++; return nil })
	var refusal *store.DeliveryAdmissionError
	require.ErrorAs(err, &refusal)
	assert.Equal("draft_required", refusal.Code)
	assert.Equal("admission_snapshot_inherited", refusal.Reason)
	assert.Zero(calls)
}

func TestDeliveryAdmissionReportsProviderFailureAndCancellation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed)
	recipients := deliveryRecipient(p, addresses[0])
	var refusal *store.DeliveryAdmissionError

	failure := errors.New("synthetic transport result unknown")
	err := f.Store.WithDeliveryAdmissionContext(t.Context(), recipients, func(context.Context) error { return failure })
	require.ErrorAs(err, &refusal)
	require.ErrorIs(err, failure)
	assert.Equal("delivery_uncertain", refusal.Code)
	assert.True(refusal.ProviderAttempted)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	err = f.Store.WithDeliveryAdmissionContext(cancelled, recipients, func(context.Context) error { calls++; return nil })
	require.ErrorAs(err, &refusal)
	require.ErrorIs(err, context.Canceled)
	assert.Equal("admission_cancelled", refusal.Code)
	assert.Zero(calls)

	// A store failure is unavailability, never a policy answer.
	_, err = f.Store.DB().Exec(`DROP TABLE delivery_policies`)
	require.NoError(err)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), recipients, func(context.Context) error { calls++; return nil })
	require.ErrorAs(err, &refusal)
	assert.Equal("admission_unavailable", refusal.Code)
	assert.Zero(calls)
}

func TestDeliveryPolicyRestrictionNeedsAnAddressThePersonHolds(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	other, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("other@example.test", "Other Person", "example.test"))
	require.NoError(err)
	deliveryContactPoint(t, f, other.ID, "theirs@example.test")
	// An address known only from extraction is not a declared contact point.
	_, err = f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "extracted@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceExtraction, Confidence: new(0.5)},
	})
	require.NoError(err)

	// A participant bound to the person and seen in archived mail is still not a saved contact point.
	seen := f.EnsureParticipant("seen-in-mail@example.test", "Seen In Mail", "example.test")
	_, err = f.Store.LinkParticipants(f.EnsureParticipant("peer@example.test", "Example Person", "example.test"), seen)
	require.NoError(err)
	seenPerson, err := f.Store.PersonForParticipants([]int64{seen})
	require.NoError(err)
	require.NotNil(seenPerson)
	assert.Equal(p.ID, seenPerson.ID)
	require.NoError(f.Store.ReplaceMessageRecipients(f.CreateMessage("seen-in-mail"), "from", []int64{seen}, []string{"Seen In Mail"}))

	// The restricting path is separate: a held but undeclared row can still be restricted.
	for _, c := range []struct {
		address  string
		policies []store.DeliveryPolicy
	}{
		{"missing@example.test", []store.DeliveryPolicy{store.DeliveryDraftOnly, store.DeliverySendAllowed}},
		{"theirs@example.test", []store.DeliveryPolicy{store.DeliveryDraftOnly, store.DeliverySendAllowed}},
		{"seen-in-mail@example.test", []store.DeliveryPolicy{store.DeliveryDraftOnly, store.DeliverySendAllowed}},
		{"extracted@example.test", []store.DeliveryPolicy{store.DeliverySendAllowed}},
	} {
		state := readDelivery(t, f.Store, p.VCardUID, c.address)
		assert.Equal(store.DeliveryDraftOnly, state.EffectivePolicy, c.address)
		for _, policy := range c.policies {
			label := c.address + "/" + string(policy)
			_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(state, policy))
			require.ErrorIs(err, store.ErrDeliveryPolicyInvalid, label)
			assert.Equal("detached_endpoint", store.DeliveryErrorCode(err), label)
		}
	}
	var stored int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM delivery_policies`).Scan(&stored))
	assert.Zero(stored)

	// Restricting cannot change on another person's spelling, so it succeeds.
	deliveryContactPoint(t, f, other.ID, "<"+addresses[0]+">")
	receipt := setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliveryDraftOnly)
	assert.Equal(store.DeliveryDraftOnly, receipt.After.EffectivePolicy)
}

func TestDeliveryPolicyWritesLeaveThePersonRevisionAlone(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	personRevision := func() (revision int64) {
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT revision FROM persons WHERE id=?`), p.ID).Scan(&revision))
		return revision
	}
	before := personRevision()

	initial := readDelivery(t, f.Store, p.VCardUID, addresses[0])
	allowed := setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed).After
	assert.Equal(before, personRevision(), "a set keeps the revision enrichment binds to")

	_, err := f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed, store.DeliveryDraftOnly))
	require.NoError(err)
	assert.Equal(before, personRevision(), "a clear keeps it too")
	_, err = f.Store.ClearDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed, store.DeliveryDraftOnly))
	require.ErrorIs(err, store.ErrDeliveryPolicyRevisionConflict, "a stale clear cannot delete")
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(initial, store.DeliverySendAllowed))
	require.ErrorIs(err, store.ErrDeliveryPolicyRevisionConflict, "a stale set cannot overwrite")
	// A missing revision decodes to zero and is refused, never matched.
	zero := deliveryWrite(initial, store.DeliveryDraftOnly)
	zero.ExpectedRevision = 0
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), zero)
	require.ErrorIs(err, store.ErrDeliveryPolicyInvalid)

	// A person edit still moves the revision a client read.
	read := readDelivery(t, f.Store, p.VCardUID, addresses[0])
	deliveryContactPoint(t, f, p.ID, "third@example.test")
	assert.Greater(personRevision(), before)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(read, store.DeliverySendAllowed))
	require.ErrorIs(err, store.ErrDeliveryPolicyRevisionConflict)
}

func TestDeliveryPolicyBindsTheLowestDuplicateContactPoint(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, addresses := deliveryPerson(t, f)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO person_contact_points(person_id, address_kind, original_value, normalized_value, normalization, source)
		SELECT person_id, address_kind, original_value, normalized_value, normalization, source FROM person_contact_points WHERE person_id=? AND normalized_value=?`), p.ID, addresses[0])
	require.NoError(err)
	var lowest, count int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT MIN(id), COUNT(*) FROM person_contact_points WHERE person_id=? AND normalized_value=?`), p.ID, addresses[0]).Scan(&lowest, &count))
	require.EqualValues(2, count)

	receipt := setDelivery(t, f.Store, p.VCardUID, addresses[0], store.DeliverySendAllowed)
	assert.Equal(lowest, *receipt.Audit.ContactPointID)
}
