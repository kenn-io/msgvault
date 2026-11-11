package store

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPeopleWorkersColdPolicyBatchesCumulativeInputs(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newPersonSweepApplyFixture(t, "policy-batch", true)
	st := f.store
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	source, err := st.GetOrCreateSource("slack", "policy-batch")
	requirements.NoError(err)
	room, err := st.EnsureConversationWithType(source.ID, "policy-batch", "direct_chat", "Synthetic batch chat")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "policy-batch", conversationpolicy.Full))
	var participant int64
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=?`), f.personID).Scan(&participant))
	// Direct fixture inserts keep 2,001 synthetic messages inexpensive. Admission
	// and evidence hydration below use the real production paths and transaction.
	tx, err := st.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	ids := make([]int64, maxPersonSweepEvidenceRows+1)
	for i := range ids {
		requirements.NoError(tx.QueryRowContext(ctx, st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,sender_id,snippet) VALUES(?,?,?,'chat',?,?) RETURNING id`), source.ID, fmt.Sprintf("batch-%d", i), room, participant, "Synthetic evidence").Scan(&ids[i]))
	}
	requirements.NoError(tx.Commit())
	var inputs []peoplesweep.EvidenceItem
	for start := 0; start < len(ids); start += maxPersonSweepEvidenceRows {
		end := min(start+maxPersonSweepEvidenceRows, len(ids))
		batch, _, err := st.hydratePersonSweepMessageSet(ctx, f.personID, peoplesweep.SourceConversationText, ids[start:end], 0)
		requirements.NoError(err)
		inputs = append(inputs, batch...)
	}
	requirements.Len(inputs, len(ids))
	captured, err := st.CapturePersonSweepPolicyWork(ctx, f.personID)
	requirements.NoError(err)
	st.DB().SetMaxOpenConns(1)
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true, MaxStmtChars: 10000})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })
	var trace bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&trace, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	count := func(items []peoplesweep.EvidenceItem) int {
		work, err := st.BindPersonSweepPolicyInputs(captured, items)
		require.NoError(t, err)
		trace.Reset()
		require.NoError(t, CheckConversationPolicyAdmission(work))
		return strings.Count(trace.String(), `"msg":"sql"`)
	}
	single := count(inputs[:1])
	requirements.Positive(single)
	eight := count(inputs[:8])
	assertions.LessOrEqual(eight, single+2, "one class should hydrate together")
	// Deduplicate repeated citations, retaining every original comparison.
	duplicate := append(append([]peoplesweep.EvidenceItem{}, inputs[:8]...), inputs[:8]...)
	assertions.LessOrEqual(count(duplicate), single+2)
	// Cumulative bound is not a new total rejection: use two bounded batches.
	assertions.LessOrEqual(count(inputs), 2*single+2)
	stale := append([]peoplesweep.EvidenceItem{}, inputs...)
	stale[len(stale)-1].ContentSHA256 = "changed-synthetic-hash"
	work, err := st.BindPersonSweepPolicyInputs(captured, stale)
	requirements.NoError(err)
	assertions.ErrorIs(CheckConversationPolicyAdmission(work), ErrConversationPolicyStale, "last item after the chunk boundary is still compared")
}
