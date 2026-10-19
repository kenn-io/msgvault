package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAppleNameUpgradeUsesOnlyExactNativePayloadEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("whatsapp", "+15555550100")
	requirements.NoError(err)
	room, err := st.EnsureConversationWithType(source.ID, "group", "group_chat", "Synthetic Group")
	requirements.NoError(err)
	participant, err := st.EnsureParticipantByPhone("+15555550101", "Unproven Shared Name", "whatsapp")
	requirements.NoError(err)
	for _, fixture := range []struct{ id, format, raw string }{
		{"contact", "whatsapp_apple_json", `{"from_me":0,"group_member_jid":"15555550101@s.whatsapp.net","group_contact_name":"Exact Native Name","group_first_name":"First Name"}`},
		{"first", "whatsapp_apple_json", `{"from_me":0,"group_member_jid":"15555550101@s.whatsapp.net","group_first_name":"Exact First Name"}`},
		{"outgoing", "whatsapp_apple_json", `{"from_me":1,"group_contact_name":"Not Sender Name"}`},
		{"other-format", "whatsapp_json", `{"from_me":0,"group_contact_name":"Wrong Format Name"}`},
		{"malformed", "whatsapp_apple_json", `{broken`},
		{"missing", "whatsapp_apple_json", `{"from_me":0}`},
	} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: fixture.id, MessageType: "whatsapp", SenderID: sql.NullInt64{Int64: participant, Valid: true}}, RawMIME: []byte(fixture.raw), RawFormat: fixture.format})
		requirements.NoError(err)
	}
	beeper, err := st.GetOrCreateSource("beeper", "synthetic-account")
	requirements.NoError(err)
	cold, err := st.EnsureConversationWithType(beeper.ID, "cold", "direct_chat", "")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), beeper.ID, "cold", "cold"))
	_, err = st.UpsertMessage(&store.Message{SourceID: beeper.ID, ConversationID: cold, SourceMessageID: "cold-name", MessageType: "beeper", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
	requirements.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='apple_whatsapp_name_provenance_v1'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	check := func() {
		rows, err := st.DB().Query(st.Rebind(`SELECT original_value FROM participant_contact_observations WHERE participant_id=? AND source_id=? AND address_kind='display_name' ORDER BY original_value`), participant, source.ID)
		requirements.NoError(err)
		defer func() { requirements.NoError(rows.Close()) }()
		var names []string
		for rows.Next() {
			var name string
			requirements.NoError(rows.Scan(&name))
			names = append(names, name)
		}
		requirements.NoError(rows.Err())
		assertions.Equal([]string{"Exact First Name", "Exact Native Name"}, names)
		requirements.NoError(rows.Close())
		for _, candidate := range []struct {
			name string
			want int
		}{
			{"Exact Native Name", 1}, {"Exact First Name", 1}, {"Unproven Shared Name", 0}, {"Wrong Format Name", 0},
		} {
			var eligible int
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM participants p WHERE p.id=? AND `+store.FullParticipantNameWhere("p.id", "?")), participant, candidate.name, candidate.name).Scan(&eligible))
			assertions.Equal(candidate.want, eligible, candidate.name)
		}
	}
	check()
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='apple_whatsapp_name_provenance_v1'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	check()
}
