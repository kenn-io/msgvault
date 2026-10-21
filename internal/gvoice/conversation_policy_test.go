package gvoice

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReimportBackfillsNativeGoogleVoiceNameEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	const ownerPhone = "+12025550100"
	const contactPhone = "+12025550101"
	const contactName = "Synthetic Contact"
	voice := t.TempDir()
	calls := filepath.Join(voice, "Calls")
	requirements.NoError(os.MkdirAll(calls, 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(voice, "Phones.vcf"), []byte(`BEGIN:VCARD
VERSION:3.0
FN:Synthetic Owner
item1.TEL:+12025550100
item1.X-ABLabel:Google Voice
TEL;TYPE=CELL:+12025550102
END:VCARD
`), 0o600))
	requirements.NoError(os.WriteFile(filepath.Join(calls, "Synthetic Contact - Text - 2020-02-03T17_37_45Z.html"), []byte(`<!DOCTYPE html>
<html><head><title>Synthetic Contact</title></head><body><div class="hChatLog hfeed">
<div class="message"><abbr class="dt" title="2020-02-03T17:36:45.000Z">Feb 3, 2020</abbr>: <cite class="sender vcard"><a class="tel" href="tel:+12025550101"></a></cite>: <q>Earlier message with no name</q></div>
<div class="message"><abbr class="dt" title="2020-02-03T17:37:45.000Z">Feb 3, 2020</abbr>: <cite class="sender vcard"><a class="tel" href="tel:+12025550101"><span class="fn">Synthetic Contact</span></a></cite>: <q>Example message</q></div>
</div></body></html>`), 0o600))

	client, err := NewClient(voice)
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", ownerPhone)
	requirements.NoError(err)
	participantID, err := st.EnsureParticipantByPhone(contactPhone, contactName, "google_voice")
	requirements.NoError(err)
	chatSource, err := st.GetOrCreateSource("beeper", "synthetic-name-support")
	requirements.NoError(err)
	chatRoom, err := st.EnsureConversation(chatSource.ID, "synthetic-room", "Synthetic room")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipantContext(ctx, chatRoom, participantID, "member"))
	requirements.NoError(st.SetConversationPolicyOverride(ctx, chatSource.ID, "synthetic-room", conversationpolicy.Cold))

	first, err := client.Import(ctx, st, source.ID)
	requirements.NoError(err)
	assertions.Equal(2, first.MessagesImported)
	observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	requirements.NoError(err)
	var nativeNames []store.ParticipantContactObservation
	for _, observation := range observations {
		if observation.AddressKind == store.ContactAddressDisplayName && observation.SourceID != nil && *observation.SourceID == source.ID {
			nativeNames = append(nativeNames, observation)
		}
	}
	requirements.Len(nativeNames, 1)
	assertions.Equal(contactName, nativeNames[0].OriginalValue)

	_, err = client.Import(ctx, st, source.ID)
	requirements.NoError(err)
	observations, err = st.ListParticipantObservationsContext(ctx, participantID, true)
	requirements.NoError(err)
	assertions.Len(observations, 1, "reimport should recover the name once and remain idempotent")
	peerID, err := st.EnsureParticipant("peer@example.test", "Synthetic Peer", "example.test")
	requirements.NoError(err)
	packet, err := st.PersonMatchPairSummariesContext(ctx, participantID, peerID)
	requirements.NoError(err)
	assertions.Equal(contactName, packet.Left.DisplayName,
		"native name evidence remains available to the summary after chat demotion")

	display := store.FullParticipantDisplaySQL("p")
	var eligibleName string
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT `+display+` FROM participants p WHERE p.id=?`), participantID).Scan(&eligibleName))
	assertions.Equal(contactName, eligibleName, "Google Voice name remains eligible after the shared Beeper room becomes Cold")
}
