package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConcurrentAddressAsLeavesOneActive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL lock contract")
	}
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, value := range []string{"Vee", "Val"} {
		go func() {
			<-start
			_, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs, ValueText: value, Text: "call me " + value, Actor: "owner"})
			results <- err
		}()
	}
	close(start)
	require.NoError(<-results)
	require.NoError(<-results)
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(err)
	assert.Len(rows, 1)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_instruction_events WHERE action='supersede'`).Scan(&count))
	assert.Equal(1, count)
}
func TestInstructionWriteConcurrentWithMergeNoDeadlock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL lock contract")
	}
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	id := addTestInstruction(t, st, bob.ID, "Vee").ID
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	decision := make(chan error, 1)
	type mergeOutcome struct {
		result *store.PersonMergeResult
		err    error
	}
	merged := make(chan mergeOutcome, 1)
	go func() {
		<-start
		_, err := st.DecidePersonInstructionContext(ctx, store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionRevoke, Actor: "owner"})
		decision <- err
	}()
	go func() {
		<-start
		result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: alice.ID, AbsorbedID: bob.ID, ExpectedSurvivorRevision: alice.Revision, ExpectedAbsorbedRevision: bob.Revision, IdempotencyKey: "instruction-race", Actor: "owner"})
		merged <- mergeOutcome{result: result, err: err}
	}()
	close(start)
	mergeResult := <-merged
	require.NoError(mergeResult.err)
	require.NotNil(mergeResult.result)
	decisionErr := <-decision
	assert.True(decisionErr == nil || errors.Is(decisionErr, store.ErrPersonNotFound), "%v", decisionErr)
	instruction, err := st.GetPersonInstructionContext(ctx, id)
	require.NoError(err)
	assert.Equal(id, instruction.ID)
	assert.Equal(mergeResult.result.Person.ID, instruction.PersonID)
	assert.Equal(1, instructionEventCount(t, st, id, "merge_moved"))
	var proposalEvents int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_instruction_events
		WHERE person_id=? AND instruction_id=? AND action='propose'`), mergeResult.result.Person.ID, id).Scan(&proposalEvents))
	assert.Equal(1, proposalEvents)
}
