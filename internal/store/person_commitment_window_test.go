package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
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
	cfg := peoplesweep.CommitmentConfig{Enabled: true, Window: 30 * loopDay, MaxItems: 60, MaxBytes: 65536, MaxOpenAnchors: 8}
	window, err := f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow, "UTC")
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
	}, got)

	profile.AllowSensitive = false
	window, err = f.Store.LoadPersonCommitmentWindow(t.Context(), person.ID, profile, cfg, loopNow.Add(time.Hour), "UTC")
	r.NoError(err)
	a.Empty(window.Sources)
}
