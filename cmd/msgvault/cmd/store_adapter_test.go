package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStoreAPIAdapterImplementsCtxMessageStore is a compile-time guard that the
// production adapter wired into api.Server satisfies the optional context-aware
// read interface. If it drifts, api.Server silently falls back to the
// background-context read path.
func TestStoreAPIAdapterImplementsCtxMessageStore(t *testing.T) {
	var adapter api.MessageStore = &storeAPIAdapter{}
	_, ok := adapter.(api.CtxMessageStore)
	require.True(t, ok, "storeAPIAdapter must implement api.CtxMessageStore")
}

var _ api.CtxMessageStore = (*storeAPIAdapter)(nil)
var _ api.MeetingImporter = (*storeAPIAdapter)(nil)

// TestStoreAPIAdapterContextReadsHonorCancellation verifies the adapter's
// context-aware read methods thread the caller's context into the underlying
// store, so an abandoned or timed-out request is cancelled instead of running
// to completion on a background context.
func TestStoreAPIAdapterContextReadsHonorCancellation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	_, err = st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")

	adapter := &storeAPIAdapter{store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = adapter.GetStatsContext(ctx)
	require.ErrorIs(err, context.Canceled, "GetStatsContext must honor a cancelled context")

	_, _, err = adapter.ListMessagesContext(ctx, 0, 10)
	require.ErrorIs(err, context.Canceled, "ListMessagesContext must honor a cancelled context")

	_, err = adapter.GetMessageContext(ctx, 1)
	require.ErrorIs(err, context.Canceled, "GetMessageContext must honor a cancelled context")

	_, err = adapter.GetMessagesSummariesByIDsContext(ctx, []int64{1})
	require.ErrorIs(err, context.Canceled, "GetMessagesSummariesByIDsContext must honor a cancelled context")
}

func TestStoreAPIAdapterMeetingImport(t *testing.T) {
	st := testutil.NewTestStore(t)
	req, err := meetingimport.DecodeRequest(strings.NewReader(`{
		"source": {
			"identifier": "local-meetings",
			"account_email": "user@example.com"
		},
		"meeting": {
			"external_id": "42",
			"started_at": "2026-07-23T18:00:00Z",
			"transcript": "Speaker 1: synthetic transcript"
		}
	}`), meetingimport.MaxRequestBytes)
	require.NoError(t, err)
	adapter := &storeAPIAdapter{
		store:           st,
		meetingImporter: meetingimport.NewImporter(st, meetingimport.Hooks{}),
	}

	result, err := adapter.ImportMeeting(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, meetingimport.StatusCreated, result.Status)
	require.NotZero(t, result.MessageID)
}
