package store_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonSweepWorkerDefersCommitmentPrepareFailureForTheRestOfRun(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newBriefWorkerEndToEndFixture(t, "commitment-prepare-retry")
	st := f.journal.store
	_, err := st.SetPersonTrackingContext(ctx, f.journal.bobPersonID, true)
	r.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
	r.NoError(err)
	r.NoError(st.AddAccountIdentity(f.journal.sourceID, "owner@example.test", "manual"))
	sourcePeople := make(map[string]int64)
	for index, participant := range []int64{f.journal.aliceID, f.journal.bobID} {
		thread, err := st.EnsureConversationWithType(f.journal.sourceID,
			fmt.Sprintf("commitment-retry-%d", index), "direct_chat", "Planning")
		r.NoError(err)
		r.NoError(st.EnsureConversationParticipant(thread, owner, "member"))
		r.NoError(st.EnsureConversationParticipant(thread, participant, "member"))
		for message := range 3 {
			messageID, err := st.PersistMessageContext(ctx, &store.MessagePersistData{
				Message: &store.Message{SourceID: f.journal.sourceID, ConversationID: thread,
					SourceMessageID: fmt.Sprintf("commitment-retry-%d-%d", index, message), MessageType: "slack",
					SenderID: sql.NullInt64{Int64: participant, Valid: true},
					SentAt:   sql.NullTime{Time: f.now.Add(-time.Hour).Add(time.Duration(message) * time.Minute), Valid: true}},
				BodyText: sql.NullString{String: fmt.Sprintf("I will send person %d draft %d.", index, message), Valid: true},
			})
			r.NoError(err)
			personID := f.journal.alicePersonID
			if participant == f.journal.bobID {
				personID = f.journal.bobPersonID
			}
			sourcePeople[fmt.Sprintf("m%d", messageID)] = personID
		}
	}
	f.config.ChangeBatchSize = 1
	f.config.HistoricalMessageCap = 1
	f.config.WorkBatchSize = 4
	// Keep admission budgets above the entire bounded multi-person run: the
	// regression is same-run retry spending, not the separate budget fence.
	f.config.Budgets.MaxRequestsPerPerson = 40
	f.config.Budgets.MaxInputTokensPerPerson = 2_000_000
	f.config.Budgets.MaxOutputTokensPerPerson = 200_000
	f.config.Budgets.MaxInputTokensPerRun = 4_000_000
	f.config.Budgets.MaxOutputTokensPerRun = 1_000_000
	f.config.Budgets.MaxInputTokensPerDay = 8_000_000
	f.config.Budgets.MaxOutputTokensPerDay = 2_000_000
	f.config.Commitments.Enabled = true
	f.config.Commitments.MaxItems = 1
	profile, err := f.config.Profile()
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile.Fingerprint, "test-owner")
	r.NoError(err)
	clock, ids, aliceCalls, bobCalls := f.now, 0, 0, 0
	f.runner.respond = func(request peoplesweep.StructuredRequest) json.RawMessage {
		if request.ProgramID != peoplesweep.CommitmentProgramID {
			return json.RawMessage(`{"claims":[]}`)
		}
		var window peoplesweep.CommitmentWindow
		r.NoError(json.Unmarshal([]byte(request.InputText[strings.LastIndex(request.InputText, "\n")+1:]), &window))
		r.NotEmpty(window.Sources)
		source := window.Sources[0]
		personID, known := sourcePeople[source.ID]
		r.True(known, "the provider's exact source locator must belong to a fixture person")
		if personID == f.journal.bobPersonID {
			bobCalls++
			return json.RawMessage(`{"promises":[]}`)
		}
		aliceCalls++
		// Change the source after the provider reads it, before store preparation.
		messageID, err := strconv.ParseInt(strings.TrimPrefix(source.ID, "m"), 10, 64)
		r.NoError(err)
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "Changed after request", messageID)
		r.NoError(err)
		return json.RawMessage(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":%q,"quote":%q,"start":%d,"end":%d,"summary":"Send draft","due_on":null}]}`,
			source.ID, source.Direction, source.Text, source.Start, source.Start+int64(len([]rune(source.Text)))))
	}
	f.worker.Config = f.config
	f.worker.Clock = func() time.Time { return clock }
	f.worker.NewID = func() string { ids++; return fmt.Sprintf("commitment-prepare-retry-%d", ids) }
	result, err := f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, Brief: peoplesweep.BriefModeSkip})
	r.NoError(err)
	a.Positive(bobCalls, "the second person must receive a provider call")
	a.Equal(1, aliceCalls, "failed optional preparation must not spend on the same unchanged window twice in one run")
	var failures int
	for _, person := range result.People {
		if person.PersonID == f.journal.alicePersonID && person.CommitmentFailureClass != "" {
			failures++
		}
	}
	a.Equal(1, failures)
}
