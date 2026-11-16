package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCommitmentWindowScopesBothSidesOfDirectConversations(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	alex := f.EnsureParticipant(loopAlex, "", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(alex)
	r.NoError(err)
	chat := persistLoopDirectChat(t, f, "direct", []loopChatMessage{
		{fromOwner: true, body: "I will send the agenda."},
		{body: "I will check the venue."},
	})
	owner := f.EnsureParticipant(loopOwner, "", "example.com")
	sam := f.EnsureParticipant("sam@example.test", "", "example.test")
	explicitOnly := persistLoopDirectChat(t, f, "explicit-only", []loopChatMessage{
		{body: "I will prepare the meeting."},
	})[0]
	r.NoError(f.Store.ReplaceMessageRecipientsContext(t.Context(), explicitOnly, "to", []int64{owner}, nil))
	r.NoError(f.Store.ReplaceConversationParticipants(conversationOf(t, f, explicitOnly), nil))
	thirdPartyChat := persistLoopDirectChat(t, f, "third-party", []loopChatMessage{
		{body: "I will circulate the minutes."},
	})[0]
	// The message names the owner, while the imported membership includes
	// another person. The general loop group heuristic sees only two peers.
	r.NoError(f.Store.ReplaceMessageRecipientsContext(t.Context(), thirdPartyChat, "to", []int64{owner}, nil))
	r.NoError(f.Store.ReplaceConversationParticipants(conversationOf(t, f, thirdPartyChat), []store.ConversationParticipantRef{
		{ParticipantID: alex, Role: "member"},
		{ParticipantID: sam, Role: "member"},
	}))
	ownerEmail := f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex},
		Body: "I will send the budget.", SentAt: loopAgo(loopDay)})
	const quoted = "Je transmettrai le résumé."
	const authored = "I will confirm the booking."
	prefix := "On Tuesday, Owner wrote:\n> " + quoted + "\n\n"
	otherEmail := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner},
		Body: prefix + authored, SentAt: loopAgo(loopDay)})
	f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner, "sam@example.test"},
		Body: "I will write to all of you.", SentAt: loopAgo(loopDay)})
	f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, ListID: "news.example.test",
		Body: "We will send the newsletter.", SentAt: loopAgo(loopDay)})
	profile := peoplesweep.ProviderProfile{SourceSince: "2026-01-01", AllowSensitive: true,
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.CommitmentConfig{Enabled: true, Window: 30 * loopDay, MaxItems: 60, MaxBytes: 65536}
	window, err := f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow)
	r.NoError(err)
	got := make(map[int64]string)
	for _, source := range window.Sources {
		got[source.MessageID] = source.Direction
		if source.MessageID == otherEmail {
			start := int64(len([]rune(prefix)))
			a.True(source.MatchesQuote(authored, start, start+int64(len([]rune(authored)))))
			quotedStart := int64(len([]rune("On Tuesday, Owner wrote:\n> ")))
			a.False(source.MatchesQuote(quoted, quotedStart, quotedStart+int64(len([]rune(quoted)))))
		}
	}
	a.Equal(map[int64]string{
		chat[0]: peoplesweep.CommitmentOwedBy, chat[1]: peoplesweep.CommitmentOwedTo,
		ownerEmail: peoplesweep.CommitmentOwedBy, otherEmail: peoplesweep.CommitmentOwedTo,
		explicitOnly: peoplesweep.CommitmentOwedTo,
	}, got)

	profile.AllowSensitive = false
	window, err = f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow.Add(time.Hour))
	r.NoError(err)
	a.Empty(window.Sources)
}

func TestCommitmentWindowBoundsEscapedEmailInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		encoded bool
	}{{"fit bound", false}, {"encoded bytes", true}, {"timezone", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			f := storetest.New(t)
			r.NoError(f.Store.AddAccountIdentity(f.Source.ID, loopOwner, "manual"))
			participant := f.EnsureParticipant(loopAlex, "", "example.test")
			person, _, err := f.Store.CreatePersonFromParticipant(participant)
			r.NoError(err)
			if tc.name == "timezone" {
				first := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "I will send the first.", SentAt: loopAgo(2 * time.Hour)})
				second := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "I will send the second.", SentAt: loopAgo(time.Hour)})
				ids := []int64{first, second}
				for i := range 120 {
					ids = append(ids, f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "I will call.", SentAt: loopAgo(time.Minute + time.Duration(i)*time.Second)}))
				}
				for _, id := range ids {
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_sweep_changes (sequence, person_id, source_lane, change_kind, message_id, recorded_at) SELECT COALESCE(MAX(sequence), 0) + 1, ?, 'conversation_text', 'upsert', ?, ? FROM person_sweep_changes`), person.ID, id, loopNow)
					r.NoError(err)
				}
				_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE person_sweep_change_clock SET sequence = (SELECT MAX(sequence) FROM person_sweep_changes) WHERE singleton = TRUE`)
				r.NoError(err)
				cfg := peoplesweep.CommitmentConfig{Enabled: true, Timezone: "UTC", Window: 30 * loopDay, MaxItems: 200, MaxBytes: 120 << 10}
				profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
				window, err := f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow)
				r.NoError(err)
				r.Len(window.Sources, len(ids))
				last := len(window.Sources) - 1
				low, high := 0, 120<<10
				for low < high {
					mid := (low + high + 1) / 2
					window.Sources[last].Text = strings.Repeat("y", mid)
					fits, err := peoplesweep.CommitmentInputFits(window)
					r.NoError(err)
					if fits {
						low = mid
					} else {
						high = mid - 1
					}
				}
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), strings.Repeat("y", low), window.Sources[last].MessageID)
				r.NoError(err)
				baseline, err := f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow)
				r.NoError(err)
				r.Len(baseline.Sources, len(ids))
				cfg.Timezone = "America/New_York"
				window, err = f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow)
				r.NoError(err)
				r.Len(window.Sources, len(ids)-1)
				r.Less(window.ThroughSequence, window.SnapshotSequence)
				return
			}
			cfg := peoplesweep.CommitmentConfig{Enabled: true, Window: 30 * loopDay, MaxItems: 60, MaxBytes: 65536}
			var large, shorter int64
			if tc.encoded {
				large = f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "I will call.\n> " + strings.Repeat("q", 30000), SentAt: loopAgo(2 * time.Hour)})
				shorter = f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "I will reply.\n> " + strings.Repeat("q", 15000), SentAt: loopAgo(time.Hour)})
				cfg.MaxBytes = 100 << 10
			} else {
				for i := range 30 {
					f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "I will send the draft.\n> " + strings.Repeat("quoted history ", 150), SentAt: loopAgo(time.Duration(i+1) * time.Hour)})
				}
			}
			profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
			window, err := f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow)
			r.NoError(err)
			r.NotEmpty(window.Sources)
			fits, err := peoplesweep.CommitmentInputFits(window)
			r.NoError(err)
			a.True(fits)
			if tc.encoded {
				r.Len(window.Sources, 1)
				a.Equal(shorter, window.Sources[0].MessageID)
				a.NotEqual(large, window.Sources[0].MessageID)
			} else {
				a.Len(window.Sources, 5, "escaped history must leave a bounded prefix for the next pass")
				a.Equal(window.Sources[len(window.Sources)-1].MessageID, window.BackfillMessageID)
			}
		})
	}

}
