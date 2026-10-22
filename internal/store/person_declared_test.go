package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

func TestPeopleDeclaredAndProfiles(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("declared@example.net", "Declared", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugAskMeAbout, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("C++ templates")}, Source: store.ProvenanceUser})
	must.NoError(err)
	values, err := f.Store.PeopleDeclaredTopics(t.Context())
	must.NoError(err)
	must.Len(values, 1)
	check.Equal("C++ templates", values[0].Value)
	check.Equal(person.ID, values[0].PersonID)
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "declared@example.net", "manual"))
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "declared-owner", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
	must.NoError(err)
	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
	must.NoError(err)
	must.Contains(profiles, person.ID)
	check.True(profiles[person.ID].IsOwner)
	check.Equal("Declared", profiles[person.ID].DisplayName)
	check.False(profiles[person.ID].ContactPresent)
	organizations, err := f.Store.PeopleEmployments(t.Context(), []int64{person.ID})
	must.NoError(err)
	check.Empty(organizations)
}

func TestPeopleDeclaredTopicsExcludesInferredValues(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("topics@example.net", "Topics", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	for _, value := range []struct {
		text   string
		source store.Provenance
	}{
		{text: "declared robotics", source: store.ProvenanceUser},
		{text: "inferred robotics", source: store.ProvenanceEnrichment},
	} {
		_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: person.ID, DefinitionSlug: store.AttributeSlugAskMeAbout,
			Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value.text}, Source: value.source,
		})
		must.NoError(err)
	}

	got, err := f.Store.PeopleDeclaredTopics(t.Context())
	must.NoError(err)
	must.Len(got, 1)
	check.Equal(person.ID, got[0].PersonID)
	check.Equal("declared robotics", got[0].Value)
}

func TestPeopleDeclaredTopicsUsesCanonicalDefinitionAfterSlugCollision(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)

	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		DELETE FROM attribute_definitions WHERE universal_id = ?
	`), store.AttributeUniversalIDAskMeAbout)
	must.NoError(err)
	var custom store.AttributeDefinitionInput
	for _, definition := range store.SeededAttributeDefinitions() {
		if definition.Slug == store.AttributeSlugAskMeAbout {
			custom = definition
			break
		}
	}
	must.NotEmpty(custom.UniversalID)
	custom.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	custom.Label = "Custom conversation field"
	custom.Ownership = store.AttributeOwnershipUser
	custom.IsDeletable = true
	_, err = f.Store.CreateAttributeDefinitionContext(t.Context(), custom)
	must.NoError(err)
	must.NoError(f.Store.EnsureSeededAttributeDefinitionsContext(t.Context()))

	var canonicalSlug string
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT slug FROM attribute_definitions WHERE universal_id = ?
	`), store.AttributeUniversalIDAskMeAbout).Scan(&canonicalSlug)
	must.NoError(err)
	check.NotEqual(store.AttributeSlugAskMeAbout, canonicalSlug)

	participant := f.EnsureParticipant("topics@example.net", "Topics", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	for _, value := range []struct {
		slug, text string
	}{
		{slug: store.AttributeSlugAskMeAbout, text: "custom value"},
		{slug: canonicalSlug, text: "canonical topic"},
	} {
		_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: person.ID, DefinitionSlug: value.slug,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value.text},
			Source: store.ProvenanceUser,
		})
		must.NoError(err)
	}

	got, err := f.Store.PeopleDeclaredTopics(t.Context())
	must.NoError(err)
	must.Len(got, 1)
	check.Equal(person.ID, got[0].PersonID)
	check.Equal("canonical topic", got[0].Value)
}

func TestPeopleProfilesNativeOwnerAndBoundParticipants(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("native-owner@example.net", "Native Owner", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	_, err = f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "native-owner", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}, IsFromMe: true})
	must.NoError(err)
	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
	must.NoError(err)
	check.True(profiles[person.ID].IsOwner)
	check.Equal([]int64{participant}, profiles[person.ID].ParticipantIDs)
}

func TestPeopleProfilesOwnerMatchesScopedAuthoritativeEdges(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("owner@example.net", "Contact", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.net")
	must.NoError(err)
	must.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.net", "manual"))
	id, err := f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "scoped", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
	must.NoError(err)
	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
	must.NoError(err)
	check.False(profiles[person.ID].IsOwner, "identity from another source must not claim this sender")
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{participant}, []string{"Contact"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE message_recipients SET email_address=? WHERE message_id=?"), "external@example.net", id)
	must.NoError(err)
	profiles, err = f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
	must.NoError(err)
	check.False(profiles[person.ID].IsOwner, "authoritative envelope must take precedence over the participant alias")
}

func TestPeopleProfilesRecipientAndRosterOwners(t *testing.T) {
	for _, tc := range []struct {
		name, conversationType, envelope string
		recipient, wantOwner             bool
	}{
		{name: "recipient owner envelope", conversationType: "email", envelope: "owner@example.net", recipient: true, wantOwner: true},
		{name: "recipient other envelope", conversationType: "email", envelope: "external@example.net", recipient: true},
		{name: "chat roster owner", conversationType: "direct_chat", wantOwner: true},
		{name: "email roster is not evidence", conversationType: "email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			f := storetest.New(t)
			address := "owner@example.net"
			if tc.recipient {
				address = "member@example.net"
			}
			participant := f.EnsureParticipant(address, "Member", "example.net")
			sender := f.EnsureParticipant("sender@example.net", "Sender", "example.net")
			person, _, err := f.Store.CreatePersonFromParticipant(participant)
			must.NoError(err)
			must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
			conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, tc.name, tc.conversationType, tc.name)
			must.NoError(err)
			message, err := f.Store.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: f.Source.ID, SourceMessageID: tc.name, MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}})
			must.NoError(err)
			if tc.recipient {
				must.NoError(f.Store.ReplaceMessageRecipients(message, "cc", []int64{participant}, []string{"Member"}))
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE message_recipients SET email_address=? WHERE message_id=? AND participant_id=?"), tc.envelope, message, participant)
				must.NoError(err)
			} else {
				must.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
			}
			profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
			must.NoError(err)
			must.Contains(profiles, person.ID)
			assert.Equal(t, tc.wantOwner, profiles[person.ID].IsOwner)
		})
	}
}

func TestPeopleProfilesRosterOwnerIgnoresNativeOutgoingMessageWithoutSender(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	conversation, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "roster-owner-native-outgoing", "direct_chat", "roster-owner-native-outgoing")
	must.NoError(err)
	must.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	messageID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  conversation,
		SourceID:        f.Source.ID,
		SourceMessageID: "roster-owner-native-outgoing-without-sender",
		MessageType:     "email",
		IsFromMe:        true,
	})
	must.NoError(err)

	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
	must.NoError(err)
	must.True(profiles[person.ID].IsOwner)

	roles, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{messageID})
	must.NoError(err)
	var ownerEdge *store.RoleEdge
	for i := range roles.Messages[messageID].Edges {
		if roles.Messages[messageID].Edges[i].ParticipantID == participant {
			ownerEdge = &roles.Messages[messageID].Edges[i]
			break
		}
	}
	must.NotNil(ownerEdge)
	check.True(ownerEdge.IsOwner)
}

func TestPeopleProfilesFromOwnershipUsesBoundAuthoritativeEdges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nativeFrom bool
		wantSender bool
		wantAlias  bool
	}{
		{name: "identity envelope is bound to its participant", wantAlias: true},
		{name: "source native sender takes precedence", nativeFrom: true, wantSender: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			f := storetest.New(t)
			sender := f.EnsureParticipant("sender@example.net", "Sender", "example.net")
			ownerAlias := f.EnsureParticipant("owner@example.net", "Owner alias", "example.net")
			senderPerson, _, err := f.Store.CreatePersonFromParticipant(sender)
			must.NoError(err)
			aliasPerson, _, err := f.Store.CreatePersonFromParticipant(ownerAlias)
			must.NoError(err)
			must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
			id, err := f.Store.UpsertMessage(&store.Message{
				ConversationID:  f.ConvID,
				SourceID:        f.Source.ID,
				SourceMessageID: "multi-author",
				MessageType:     "email",
				SenderID:        sql.NullInt64{Int64: sender, Valid: true},
				IsFromMe:        tc.nativeFrom,
			})
			must.NoError(err)
			must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender, ownerAlias}, []string{"Sender", "Owner alias"}))
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address = CASE participant_id WHEN ? THEN ? ELSE ? END WHERE message_id = ? AND recipient_type = 'from'`), sender, "sender@example.net", "owner@example.net", id)
			must.NoError(err)

			profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{senderPerson.ID, aliasPerson.ID})
			must.NoError(err)
			check.Equal(tc.wantSender, profiles[senderPerson.ID].IsOwner)
			check.Equal(tc.wantAlias, profiles[aliasPerson.ID].IsOwner)
		})
	}
}

func TestPeopleProfilesNativeMessageWithOneFromParticipantAndNoSenderIsOwned(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("native@example.net", "Native sender", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	recipient := f.EnsureParticipant("recipient@example.net", "Recipient", "example.net")
	recipientPerson, _, err := f.Store.CreatePersonFromParticipant(recipient)
	must.NoError(err)
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "native-no-sender",
		MessageType:     "email",
		IsFromMe:        true,
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{participant}, []string{"Native sender"}))
	must.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, []string{"Recipient"}))

	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID, recipientPerson.ID})
	must.NoError(err)
	check.True(profiles[person.ID].IsOwner)
	check.False(profiles[recipientPerson.ID].IsOwner, "native sender ownership must not apply to unrelated recipient rows")
}

func TestPeopleNativeMultipleFromRowsUseMatchingAccountIdentityWithoutSender(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	external := f.EnsureParticipant("external@example.net", "External sender", "example.net")
	owner := f.EnsureParticipant("owner@example.net", "Account owner", "example.net")
	externalPerson, _, err := f.Store.CreatePersonFromParticipant(external)
	must.NoError(err)
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	must.NoError(err)
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "native-multiauthor-without-sender",
		MessageType:     "email",
		IsFromMe:        true,
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{external, owner}, []string{"External sender", "Account owner"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients
		SET email_address = CASE participant_id WHEN ? THEN ? ELSE ? END
		WHERE message_id = ? AND recipient_type = 'from'`), external, "external@example.net", "owner@example.net", id)
	must.NoError(err)

	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{externalPerson.ID, ownerPerson.ID})
	must.NoError(err)
	check.False(profiles[externalPerson.ID].IsOwner)
	check.True(profiles[ownerPerson.ID].IsOwner, "the matching account envelope identifies one owner among multiple From rows")

	roles, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	edges := make(map[int64]store.RoleEdge, len(roles.Messages[id].Edges))
	for _, edge := range roles.Messages[id].Edges {
		edges[edge.ParticipantID] = edge
	}
	must.Contains(edges, external)
	must.Contains(edges, owner)
	check.False(edges[external].IsOwner)
	check.True(edges[owner].IsOwner, "the matching account envelope identifies one owner among multiple From rows")
}

func TestPeopleProfilesBlankFromAliasRequiresNoOtherUsableFromEnvelope(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	ownerAlias := f.EnsureParticipant("owner@example.net", "Merged contact", "example.net")
	external := f.EnsureParticipant("external@example.net", "External sender", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(ownerAlias)
	must.NoError(err)
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "mixed-from-envelopes",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: external, Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{external, ownerAlias}, []string{"External sender", "Merged contact"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address = CASE participant_id WHEN ? THEN ? ELSE NULL END WHERE message_id = ? AND recipient_type = 'from'`), external, "external@example.net", id)
	must.NoError(err)

	profiles, err := f.Store.PeopleProfiles(t.Context(), []int64{person.ID})
	must.NoError(err)
	check.False(profiles[person.ID].IsOwner)
}
