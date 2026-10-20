package synctechsms

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const (
	tierRemotePhone = "+15551234567"
	tierOwnerPhone  = "+15550000001"
)

// tierDayMillis is noon UTC, n days before today, as a backup timestamp.
func tierDayMillis(n int) int64 {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -n).Add(12 * time.Hour).UnixMilli()
}

func tierSMS(day int, smsType int, body string) string {
	return fmt.Sprintf(`<sms address="%s" date="%d" type="%d" body="%s" read="1" status="-1" contact_name="Ana" />`,
		tierRemotePhone, tierDayMillis(day), smsType, body)
}

func tierCall(day, callType int) string {
	return fmt.Sprintf(`<call number="%s" duration="0" date="%d" type="%d" presentation="1" contact_name="Ana" />`,
		tierRemotePhone, tierDayMillis(day), callType)
}

// explainImportedTier imports the backup files through the real importer,
// projects activity, and explains the tier of the person behind the remote
// phone number.
func explainImportedTier(t *testing.T, files map[string]string) *store.PersonTierExplanation {
	t.Helper()
	f := storetest.New(t)
	dir := t.TempDir()
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
	}
	source, err := f.Store.GetOrCreateSource(SourceType, tierOwnerPhone)
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(source.ID, tierOwnerPhone, "manual"))
	_, err = NewImporter(f.Store, ImportOptions{
		OwnerPhone: tierOwnerPhone, IncludeSMS: true, IncludeMMS: true, IncludeCalls: true,
	}).ImportPath(dir)
	require.NoError(t, err)
	participant, err := f.Store.EnsureParticipantByPhone(tierRemotePhone, "Ana", ParticipantIdentifierType)
	require.NoError(t, err)
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 500, MaxDirectCounterparts: 25,
	})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
	explanation, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID,
		store.PersonTierOptions{Config: persontiers.DefaultConfig()})
	require.NoError(t, err)
	return explanation
}

// Four texts from Ana and four of the owner's, on eight days. Only texts that
// left the phone are contact; the other box types never did.
func TestPersonTiersCountOnlyTextsThatWereSent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ownType  int
		wantTier int
		wantOut  int
	}{
		{"sent", 2, 1, 4},
		{"draft", 3, 3, 0},
		{"outbox", 4, 3, 0},
		{"failed", 5, 3, 0},
		{"queued", 6, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var texts []string
			for i := range 4 {
				texts = append(texts,
					tierSMS(i*2+1, 1, fmt.Sprintf("from ana %d", i)),
					tierSMS(i*2+2, tc.ownType, fmt.Sprintf("from me %d", i)))
			}
			explanation := explainImportedTier(t, map[string]string{
				"messages.xml": `<smses count="8">` + strings.Join(texts, "\n") + `</smses>`,
			})
			assert.Equal(t, tc.wantTier, explanation.Tier)
			assert.Equal(t, tc.wantOut, explanation.Signals.OutboundLong)
			assert.Equal(t, 4, explanation.Signals.InboundLong)
		})
	}
}

func TestPersonTiersCountOnlyMMSThatWereSent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		box      int
		wantTier int
	}{
		{"sent", 2, 1},
		{"draft", 3, 3},
		{"outbox", 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var texts []string
			for i := range 4 {
				texts = append(texts, tierSMS(i*2+1, 1, fmt.Sprintf("from ana %d", i)),
					fmt.Sprintf(`<mms date="%d" msg_box="%d" read="1" m_id="own-%d" sub="null">
<parts><part seq="0" ct="text/plain" text="mms %d" /></parts>
<addrs><addr address="%s" type="137" charset="106" /><addr address="%s" type="151" charset="106" /></addrs>
</mms>`, tierDayMillis(i*2+2), tc.box, i, i, tierOwnerPhone, tierRemotePhone))
			}
			explanation := explainImportedTier(t, map[string]string{
				"messages.xml": `<smses count="8">` + strings.Join(texts, "\n") + `</smses>`,
			})
			assert.Equal(t, tc.wantTier, explanation.Tier)
		})
	}
}

// Four outgoing calls with no duration and four missed calls are not messages
// exchanged with the person.
func TestPersonTiersIgnoreCallRecords(t *testing.T) {
	var calls []string
	for i := range 4 {
		calls = append(calls, tierCall(i*2+1, 2), tierCall(i*2+2, 3))
	}
	explanation := explainImportedTier(t, map[string]string{
		"calls.xml": `<calls count="8">` + strings.Join(calls, "\n") + `</calls>`,
	})

	assert.Equal(t, 3, explanation.Tier)
	assert.Zero(t, explanation.Signals.OutboundLong)
	assert.Zero(t, explanation.Signals.InboundLong)
	assert.Zero(t, explanation.Signals.ActiveDaysLong)
}

// A backup imported before send state was recorded holds the draft as sent.
// Importing the same record again corrects it.
func TestReimportRewritesSendState(t *testing.T) {
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="1">`+tierSMS(1, 3, "draft")+`</smses>`)
	imp := NewImporter(f.Store, ImportOptions{OwnerPhone: tierOwnerPhone, IncludeSMS: true})
	_, err := imp.ImportPath(dir)
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET is_sent = TRUE`)
	require.NoError(t, err)

	_, err = imp.ImportPath(dir)
	require.NoError(t, err)

	var sent bool
	require.NoError(t, f.Store.DB().QueryRow(`SELECT is_sent FROM messages WHERE message_type = 'sms'`).Scan(&sent))
	assert.False(t, sent)
}
