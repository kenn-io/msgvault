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
	tierRemote = "+15551234567"
	tierOwner  = "+15550000001"
)

// tierMillis is noon UTC, n days before today, as a backup timestamp.
func tierMillis(n int) int64 {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n).Add(12 * time.Hour).UnixMilli()
}

func tierSMS(day, smsType int, body string) string {
	return fmt.Sprintf(`<sms address="%s" date="%d" type="%d" body="%s" read="1" status="-1" />`, tierRemote, tierMillis(day), smsType, body)
}

func tierMMS(day, box int, id string) string {
	return fmt.Sprintf(`<mms date="%d" msg_box="%d" read="1" m_id="%s" sub="null"><parts><part seq="0" ct="text/plain" text="%s" /></parts>`+
		`<addrs><addr address="%s" type="137" charset="106" /><addr address="%s" type="151" charset="106" /></addrs></mms>`,
		tierMillis(day), box, id, id, tierOwner, tierRemote)
}

func tierCall(day, callType int) string {
	return fmt.Sprintf(`<call number="%s" duration="0" date="%d" type="%d" presentation="1" />`, tierRemote, tierMillis(day), callType)
}

// importedTierSignals imports a backup through the real importer, projects
// activity, and reads the tier counts of the person behind the remote phone.
func importedTierSignals(t *testing.T, file, xml string) persontiers.Signals {
	t.Helper()
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, file), xml)
	source, err := f.Store.GetOrCreateSource(SourceType, tierOwner)
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(source.ID, tierOwner, "manual"))
	_, err = NewImporter(f.Store, ImportOptions{OwnerPhone: tierOwner, IncludeSMS: true, IncludeMMS: true, IncludeCalls: true}).ImportPath(dir)
	require.NoError(t, err)
	participant, err := f.Store.EnsureParticipantByPhone(tierRemote, "Ana", ParticipantIdentifierType)
	require.NoError(t, err)
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 500, MaxDirectCounterparts: 25})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
	explanation, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, store.PersonTierOptions{Config: persontiers.DefaultConfig()})
	require.NoError(t, err)
	return explanation.Signals
}

// Four texts from Ana and four of the owner's on eight days. Sent texts count
// both ways. Drafts, outbox texts, and call records count for nothing.
func TestPersonTiersCountOnlyMessagesThatWereExchanged(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		own                       func(day int) string
		wantOutbound, wantInbound int
		wantDays                  int
	}{
		{"sent sms", func(d int) string { return tierSMS(d, 2, fmt.Sprint("sent", d)) }, 4, 4, 8},
		{"draft sms", func(d int) string { return tierSMS(d, 3, fmt.Sprint("draft", d)) }, 0, 4, 4},
		{"sent mms", func(d int) string { return tierMMS(d, 2, fmt.Sprint("mms", d)) }, 4, 4, 8},
		{"outbox mms", func(d int) string { return tierMMS(d, 4, fmt.Sprint("mms", d)) }, 0, 4, 4},
		{"failed mms", func(d int) string { return tierMMS(d, 5, fmt.Sprint("mms", d)) }, 0, 4, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var texts []string
			for i := range 4 {
				texts = append(texts, tierSMS(i*2+1, 1, fmt.Sprint("from ana", i)), tc.own(i*2+2))
			}
			signals := importedTierSignals(t, "messages.xml", `<smses>`+strings.Join(texts, "")+`</smses>`)
			assert.Equal(t, tc.wantOutbound, signals.OutboundLong)
			assert.Equal(t, tc.wantInbound, signals.InboundLong)
			assert.Equal(t, tc.wantDays, signals.ActiveDaysLong)
		})
	}
	t.Run("calls", func(t *testing.T) {
		var calls []string
		for i := range 4 {
			calls = append(calls, tierCall(i*2+1, 2), tierCall(i*2+2, 3))
		}
		signals := importedTierSignals(t, "calls.xml", `<calls>`+strings.Join(calls, "")+`</calls>`)
		assert.Zero(t, signals.OutboundLong+signals.InboundLong+signals.ActiveDaysLong)
	})
}

// Importing a record again corrects a draft stored as sent by an older import.
func TestReimportRewritesSendState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses>`+tierSMS(1, 3, "draft")+`</smses>`)
	imp := NewImporter(f.Store, ImportOptions{OwnerPhone: tierOwner, IncludeSMS: true})
	_, err := imp.ImportPath(dir)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET is_sent = TRUE`)
	require.NoError(err)
	_, err = imp.ImportPath(dir)
	require.NoError(err)

	var sent bool
	require.NoError(f.Store.DB().QueryRow(`SELECT is_sent FROM messages WHERE message_type = 'sms'`).Scan(&sent))
	assert.False(sent)
}
