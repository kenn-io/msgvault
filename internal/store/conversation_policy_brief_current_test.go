package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPeopleWorkersColdPolicyWithdrawsPersistedCurrentBrief(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newBriefWorkerEndToEndFixture(t, "policy-current")
	st := f.journal.store
	ctx := t.Context()
	st.DB().SetMaxOpenConns(1)
	person := f.journal.alicePersonID
	result, err := f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: person, Limit: 1, Brief: peoplesweep.BriefModeForce})
	requirements.NoError(err)
	requirements.Equal(1, result.PeopleSucceeded)
	current, err := st.GetPersonBriefContext(ctx, person, 0)
	requirements.NoError(err)
	requirements.NotEmpty(current.RenderedText)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, f.journal.sourceID, "person-sweep-chat", conversationpolicy.Cold))
	_, err = st.GetPersonBriefContext(ctx, person, 0)
	requirements.ErrorIs(err, store.ErrPersonBriefNotFound)
	history, err := st.GetPersonBriefContext(ctx, person, current.Version)
	requirements.NoError(err)
	assertions.Equal(current.RenderedText, history.RenderedText)
	assertions.Equal(store.PersonBriefStatusSuperseded, history.Status)
	pointers, err := st.ListPersonBriefEvidenceContext(ctx, current.ID)
	requirements.NoError(err)
	requirements.NotEmpty(pointers)
	for _, pointer := range pointers {
		assertions.False(pointer.Supported)
	}
	requirements.NoError(st.SetConversationPolicyOverride(ctx, f.journal.sourceID, "person-sweep-chat", conversationpolicy.Full))
	_, err = st.GetPersonBriefContext(ctx, person, 0)
	requirements.ErrorIs(err, store.ErrPersonBriefNotFound, "promotion cannot resurrect a superseded brief")
}

func TestPeopleWorkersColdPolicyRejectsInFlightBrief(t *testing.T) {
	for _, mode := range []string{"policy", "input", "before_request"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)

			f := newBriefWorkerEndToEndFixture(t, "policy-late-"+mode)
			st := f.journal.store
			ctx := t.Context()
			st.DB().SetMaxOpenConns(1)
			change := func() {
				if mode == "input" {
					_, err := st.DB().ExecContext(ctx, `UPDATE message_bodies SET body_text='Changed synthetic input'`)
					require.NoError(t, err)
					return
				}
				require.NoError(t, st.SetConversationPolicyOverride(ctx, f.journal.sourceID, "person-sweep-chat", conversationpolicy.Cold))
				require.NoError(t, st.SetConversationPolicyOverride(ctx, f.journal.sourceID, "person-sweep-chat", conversationpolicy.Full))
			}
			if mode == "before_request" {
				f.runner.beforePrepare = change
			} else {
				f.runner.afterBrief = change
			}
			result, err := f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
			require.ErrorIs(t, err, store.ErrConversationPolicyStale)
			assertions.Zero(result.PeopleSucceeded, "old provider response cannot publish")
			_, err = st.GetPersonBriefContext(ctx, f.journal.alicePersonID, 0)
			require.ErrorIs(t, err, store.ErrPersonBriefNotFound)
			if mode == "before_request" {
				assertions.Zero(f.runner.extractionRequests)
				assertions.Zero(f.runner.briefRequests)
			}
		})
	}
}

// Exercise the actual public commit with a fresh caller context, as durable
// callers do; the original cited evidence must still be checked in its TX.
type coldBriefCommitSink struct {
	*store.Store

	before func()
	ctx    context.Context
}

func (s coldBriefCommitSink) ApplyPersonSweep(_ context.Context, request peoplesweep.ApplyRequest) (peoplesweep.ApplyResult, error) {
	s.before()
	return s.Store.ApplyPersonSweep(s.ctx, request)
}

func TestPeopleWorkersColdPolicyBriefCommitRechecksReferencedEvidence(t *testing.T) {
	f := newBriefWorkerEndToEndFixture(t, "policy-public-commit")
	st := f.journal.store
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	st.DB().SetMaxOpenConns(1)
	f.worker.Sink = coldBriefCommitSink{Store: st, ctx: ctx, before: func() {
		require.NoError(t, st.SetConversationPolicyOverride(ctx, f.journal.sourceID, "person-sweep-chat", conversationpolicy.Cold))
	}}
	_, err := f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	_, err = st.GetPersonBriefContext(ctx, f.journal.alicePersonID, 0)
	require.ErrorIs(t, err, store.ErrPersonBriefNotFound)
}
