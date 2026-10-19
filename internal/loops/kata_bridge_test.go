package loops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

type testKataConnector struct {
	kata     KataIssues
	err      error
	calls    int
	wait     bool
	deadline time.Time
	connect  func(context.Context) (KataIssues, error)
}

func (c *testKataConnector) Connect(ctx context.Context) (KataIssues, error) {
	c.calls++
	c.deadline, _ = ctx.Deadline()
	if c.connect != nil {
		return c.connect(ctx)
	}
	if c.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.kata, c.err
}
func bridgeFixture(t *testing.T) (*Service, *loopKataClient, *Bridge, *testKataConnector, time.Time) {
	t.Helper()
	s, _, now, _ := serviceFixture(t)
	k := wireLoopKata(t, s)
	_, err := s.Track(t.Context(), 1, TrackRequest{})
	require.NoError(t, err)
	connector := &testKataConnector{kata: k}
	b, err := NewBridge(s.st, connector, &s.cfg.Loops.Kata)
	require.NoError(t, err)
	b.now = func() time.Time { return now }
	return s, k, b, connector, now
}

func TestFailedBridgeRefreshPreservesNewerCache(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	oldChecked := now.Add(-time.Hour)
	link.CheckedAt = &oldChecked
	require.NoError(t, s.st.UpdateLoopKataLinkContext(t.Context(), link))
	original := k.Service.Get
	k.get = func(ctx context.Context, ref string) (taskclient.KataTask, error) {
		k.get = nil
		defer func() { k.get = nil }()
		issue, err := original(ctx, ref)
		if err != nil {
			return issue, err
		}
		k.get = func(context.Context, string) (taskclient.KataTask, error) {
			issue.Title = "Newer detail title"
			issue.Status = "closed"
			issue.WebURL = "https://example.test/issues/detail"
			return issue, nil
		}
		_, err = s.Get(ctx, 1)
		if err != nil {
			return taskclient.KataTask{}, err
		}
		return taskclient.KataTask{}, errors.New("bridge remote unavailable")
	}
	result, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)
	got, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "Newer detail title", got.IssueTitle)
	assert.Equal(t, "closed", got.IssueStatus)
	assert.Equal(t, "https://example.test/issues/detail", got.IssueWebURL)
	require.NotNil(t, got.CheckedAt)
	assert.True(t, now.Equal(*got.CheckedAt))
	assert.Equal(t, 1, got.Attempts)
	require.NotNil(t, got.NextAttemptAt)
	assert.True(t, now.Add(time.Minute).Equal(*got.NextAttemptAt))
	assert.NotEmpty(t, got.LastError)
}

func TestBridgeStorageFailureDiagnostics(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	original := k.Service.Get
	k.get = func(ctx context.Context, ref string) (taskclient.KataTask, error) {
		issue, err := original(ctx, ref)
		issue.Status = "closed"
		return issue, err
	}
	_, err := s.st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_open_loop_close
		BEFORE UPDATE ON open_loops WHEN OLD.id = 1
		BEGIN SELECT RAISE(ABORT, 'archive write failed'); END`)
	require.NoError(t, err)

	result, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "kata bridge storage failed", link.LastError)
	require.NotNil(t, link.NextAttemptAt)
	state, err := s.st.LoopDerivationStateContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "kata bridge storage failed", state.KataLastError)
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "open", row.Status)
	assert.True(t, now.Before(*link.NextAttemptAt))
}

func TestBridgeClosesAndReopensOnlyItsOwnLoop(t *testing.T) {
	s, k, b, c, now := bridgeFixture(t)
	original := k.Service.Get
	k.get = func(ctx context.Context, r string) (taskclient.KataTask, error) {
		issue, err := original(ctx, r)
		issue.Status = "closed"
		return issue, err
	}
	result, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Processed)
	assert.Equal(t, 1, c.calls)
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "closed", row.Status)
	require.NotNil(t, row.ClosedBy)
	assert.Equal(t, "system", *row.ClosedBy)
	require.NotNil(t, row.Kata.BridgeClosedLoopAt)
	assert.Contains(t, string(row.EvidenceJSON), "kata_closed_ref")
	k.get = func(ctx context.Context, r string) (taskclient.KataTask, error) {
		issue, err := original(ctx, r)
		issue.Status = "open"
		return issue, err
	}
	b.now = func() time.Time { return now.Add(time.Hour) }
	_, err = b.RunOnce(t.Context())
	require.NoError(t, err)
	row, err = s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "open", row.Status)
	assert.Nil(t, row.Kata.BridgeClosedLoopAt)
	var reason string
	require.NoError(t, s.st.DB().QueryRow(s.st.Rebind("SELECT reason FROM loop_conversation_queue WHERE conversation_id=?"), row.ConversationID).Scan(&reason))
	assert.Equal(t, "reopen", reason)
}
func TestBridgeOwnerActionsWinDuringKataRead(t *testing.T) {
	for _, action := range []string{"done", "untrack", "same issue relink"} {
		t.Run(action, func(t *testing.T) {
			s, k, b, _, now := bridgeFixture(t)
			original := k.Service.Get
			k.get = func(ctx context.Context, r string) (taskclient.KataTask, error) {
				issue, err := original(ctx, r)
				if err != nil {
					return issue, err
				}
				switch action {
				case "done":
					_, err = s.st.CloseLoopContext(ctx, 1, "handled", now.Add(time.Second))
				case "untrack":
					err = s.Untrack(ctx, 1)
				case "same issue relink":
					link, e := s.st.GetLoopKataLinkContext(ctx, 1)
					require.NoError(t, e)
					require.NoError(t, s.Untrack(ctx, 1))
					link.LinkToken = ""
					_, _, err = s.st.InsertLoopKataLinkContext(ctx, link, "")
				}
				require.NoError(t, err)
				issue.Status = "closed"
				return issue, nil
			}
			_, err := b.RunOnce(t.Context())
			require.NoError(t, err)
			row, err := s.st.GetOpenLoopContext(t.Context(), 1)
			require.NoError(t, err)
			if action == "done" {
				assert.Equal(t, "user", *row.ClosedBy)
			} else {
				assert.Equal(t, "open", row.Status)
			}
		})
	}
}
func TestBridgeCommentsDeduplicateByStableMarker(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	_, err := s.st.DB().Exec(s.st.Rebind("UPDATE open_loops SET kind='awaiting_reply',status='closed',closed_reason='turn_flip',closed_by='system',closed_at=?,closed_message_id=anchor_message_id WHERE id=1"), now)
	require.NoError(t, err)
	var event, body string
	original := k.Service.Comment
	k.comment = func(ctx context.Context, uid, key, text string) error {
		event, body = key, text
		return original(ctx, uid, key, text)
	}
	_, err = b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Contains(t, body, "A reply arrived on 2026-10-17")
	assert.Contains(t, body, "msgvault:lc:")
	assert.Len(t, event, 64)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, link.ClosedNotifiedAt)
	// Simulate loss of the acknowledgement after the comment reached Kata.
	_, err = s.st.DB().Exec("UPDATE loop_kata_links SET closed_notified_at=NULL")
	require.NoError(t, err)
	_, err = s.st.DB().Exec("UPDATE persons SET display_name='Renamed Person' WHERE id=(SELECT counterparty_person_id FROM open_loops WHERE id=1)")
	require.NoError(t, err)
	b.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	k.comment = func(context.Context, string, string, string) error {
		return errors.New("marker should suppress a second comment")
	}
	_, err = b.RunOnce(t.Context())
	require.NoError(t, err)
	link, err = s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, link.ClosedNotifiedAt)
	assert.Zero(t, link.Attempts)
}
func TestBridgeErrorsBackoffAndMissingRemainIsolated(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "missing"}[missing], func(t *testing.T) {
			s, k, b, c, now := bridgeFixture(t)
			k.get = func(context.Context, string) (taskclient.KataTask, error) {
				if missing {
					return taskclient.KataTask{}, taskclient.ErrNotFound
				}
				return taskclient.KataTask{}, errors.New("secret issue title must not be recorded")
			}
			_, err := b.RunOnce(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, c.calls)
			link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
			require.NoError(t, err)
			if missing {
				assert.Equal(t, "missing", link.IssueStatus)
				assert.Zero(t, link.Attempts)
				assert.Equal(t, now.Add(6*time.Hour), *link.NextAttemptAt)
			} else {
				assert.Equal(t, 1, link.Attempts)
				assert.Equal(t, now.Add(time.Minute), *link.NextAttemptAt)
			}
			assert.NotContains(t, link.LastError, "secret")
			row, err := s.st.GetOpenLoopContext(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, "open", row.Status)
		})
	}
}
func TestBridgeConnectionFailureAndBudgetRecordState(t *testing.T) {
	s, _, b, c, now := bridgeFixture(t)
	c.err = errors.New("a secret endpoint")
	_, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	state, err := s.st.LoopDerivationStateContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, now, *state.KataLastRunAt)
	assert.Nil(t, state.KataLastSuccessAt)
	assert.NotEmpty(t, state.KataLastError)
	assert.NotContains(t, state.KataLastError, "secret")
	c.err = nil
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := b.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, result.Processed)
	state, err = s.st.LoopDerivationStateContext(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, state.KataLastSuccessAt)
}
func TestBridgeDisabledActionsAndBatch(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	_, err := s.Track(t.Context(), 2, TrackRequest{})
	require.NoError(t, err)
	b.cfg.Batch = 1
	b.cfg.CloseOnIssueClosed = false
	original := k.Service.Get
	k.get = func(ctx context.Context, ref string) (taskclient.KataTask, error) {
		issue, err := original(ctx, ref)
		issue.Status = "closed"
		return issue, err
	}
	passNow := now.Add(time.Minute)
	b.now = func() time.Time { return passNow }
	firstPass, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, firstPass.Processed)
	first, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, passNow, *first.CheckedAt)
	live, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "open", live.Status)
	assert.Equal(t, "closed", live.Kata.IssueStatus)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 2)
	require.NoError(t, err)
	assert.Equal(t, now, *link.NextAttemptAt)
	_, err = s.st.DB().Exec(s.st.Rebind("UPDATE open_loops SET status='closed',closed_reason='turn_flip',closed_by='system',closed_at=? WHERE id IN (1,2)"), now)
	require.NoError(t, err)
	b.cfg.CommentOnClose = false
	k.get = func(context.Context, string) (taskclient.KataTask, error) {
		return taskclient.KataTask{}, errors.New("disabled comments must not be fetched")
	}
	result, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Zero(t, result.Processed)
}

func TestLoopCommentArchiveAndKind(t *testing.T) {
	s, _, _, _, now := bridgeFixture(t) //nolint:dogsled // this case uses only the store and fixture clock
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	row.ClosedAt = &now
	row.ClosedMessageID = row.AnchorMessageID
	first, body := loopClosureComment("archive-one", row)
	second, _ := loopClosureComment("archive-two", row)
	assert.NotEqual(t, first, second)
	assert.Contains(t, body, "You replied")
	row.Kind = AwaitingReply
	row.CounterpartyDisplayName = "Saved recipient"
	_, body = loopClosureComment("archive-one", row)
	assert.True(t, strings.HasPrefix(body, "A reply arrived on"))
	assert.NotContains(t, body, "Saved recipient", "a different group recipient may have replied")
}

func TestBridgeFailedRowDoesNotStarveHealthyRow(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	tracked, err := s.Track(t.Context(), 2, TrackRequest{})
	require.NoError(t, err)
	original := k.Service.Get
	k.get = func(ctx context.Context, r string) (taskclient.KataTask, error) {
		if r != tracked.Issue.UID {
			return taskclient.KataTask{}, context.DeadlineExceeded
		}
		return original(ctx, r)
	}
	later := now.Add(time.Minute)
	b.now = func() time.Time { return later }
	result, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, result.Processed)
	assert.Equal(t, 1, result.Failed)
	failed, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, failed.Attempts)
	assert.Equal(t, later.Add(time.Minute), *failed.NextAttemptAt)
	healthy, err := s.st.GetLoopKataLinkContext(t.Context(), 2)
	require.NoError(t, err)
	assert.Equal(t, later, *healthy.CheckedAt)
	assert.Zero(t, healthy.Attempts)
	state, err := s.st.LoopDerivationStateContext(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, state.KataLastError)
	assert.Nil(t, state.KataLastSuccessAt)
}
func TestBridgeCommentFailureRetriesAndDeletionStopsComments(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "deleted during comment"}[deleted], func(t *testing.T) {
			s, k, b, _, now := bridgeFixture(t)
			_, err := s.st.DB().Exec(s.st.Rebind("UPDATE open_loops SET status='closed',closed_reason='turn_flip',closed_by='system',closed_at=? WHERE id=1"), now)
			require.NoError(t, err)
			k.comment = func(context.Context, string, string, string) error {
				if deleted {
					return taskclient.ErrNotFound
				}
				return errors.New("private service error")
			}
			_, err = b.RunOnce(t.Context())
			require.NoError(t, err)
			link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
			require.NoError(t, err)
			assert.Nil(t, link.ClosedNotifiedAt)
			assert.NotContains(t, link.LastError, "private")
			if deleted {
				assert.Equal(t, "missing", link.IssueStatus)
				assert.Equal(t, now.Add(6*time.Hour), *link.NextAttemptAt)
			} else {
				assert.Equal(t, 1, link.Attempts)
			}
		})
	}
}

func TestBridgeConnectReservesStateWriteMargin(t *testing.T) {
	s, _, b, c, _ := bridgeFixture(t)
	c.wait = true
	ctx, cancel := context.WithTimeout(t.Context(), kataStopMargin+time.Millisecond)
	defer cancel()
	_, err := b.RunOnce(ctx)
	require.NoError(t, err)
	deadline, _ := ctx.Deadline()
	assert.Equal(t, deadline.Add(-kataStopMargin), c.deadline)
	state, err := s.st.LoopDerivationStateContext(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, state.KataLastRunAt)
	assert.Nil(t, state.KataLastSuccessAt)
	assert.NotEmpty(t, state.KataLastError)
}

func TestBridgeUnknownIssueStatusDoesNotReopen(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	original := k.Service.Get
	remoteStatus := "closed"
	k.get = func(ctx context.Context, r string) (taskclient.KataTask, error) {
		issue, err := original(ctx, r)
		issue.Status = remoteStatus
		return issue, err
	}
	_, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	remoteStatus = "unknown"
	b.now = func() time.Time { return now.Add(time.Hour) }
	_, err = b.RunOnce(t.Context())
	require.NoError(t, err)
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "closed", row.Status)
	require.NotNil(t, row.Kata.BridgeClosedLoopAt)
}

func TestBridgeRowTimeoutPersistsBackoff(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	k.get = func(ctx context.Context, _ string) (taskclient.KataTask, error) {
		<-ctx.Done()
		return taskclient.KataTask{}, ctx.Err()
	}
	result, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, link.Attempts)
	assert.Equal(t, now.Add(time.Minute), *link.NextAttemptAt)
	assert.Equal(t, "request to Kata timed out", link.LastError)
	state, err := s.st.LoopDerivationStateContext(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, state.KataLastRunAt)
	assert.NotEmpty(t, state.KataLastError)
}
func TestBridgeBackoffStepsAndSuccessReset(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	k.get = func(context.Context, string) (taskclient.KataTask, error) {
		return taskclient.KataTask{}, errors.New("Kata unavailable")
	}
	current := now
	for i, delay := range []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour} {
		b.now = func() time.Time { return current }
		_, err := b.RunOnce(t.Context())
		require.NoError(t, err)
		link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
		require.NoError(t, err)
		assert.Equal(t, i+1, link.Attempts)
		assert.Equal(t, current.Add(delay), *link.NextAttemptAt)
		current = *link.NextAttemptAt
	}
	k.get = nil
	b.now = func() time.Time { return current }
	_, err := b.RunOnce(t.Context())
	require.NoError(t, err)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Zero(t, link.Attempts)
	assert.Empty(t, link.LastError)
	assert.Equal(t, current, *link.NextAttemptAt)
}

func TestBridgeCanceledReadStillRecordsBackoff(t *testing.T) {
	s, k, b, _, now := bridgeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := k.Service.Get
	k.get = func(readCtx context.Context, ref string) (taskclient.KataTask, error) {
		issue, err := original(readCtx, ref)
		cancel()
		return issue, err
	}
	_, err := b.RunOnce(ctx)
	require.NoError(t, err)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, link.Attempts)
	assert.Equal(t, now.Add(time.Minute), *link.NextAttemptAt)
}

func TestBridgeCanceledAdmissionDoesNotWaitForActivePass(t *testing.T) {
	_, _, b, c, _ := bridgeFixture(t) //nolint:dogsled // this case exercises the bridge and connector only
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	c.connect = func(context.Context) (KataIssues, error) { entered <- struct{}{}; <-release; return c.kata, nil }
	firstDone := make(chan error, 1)
	go func() { _, err := b.RunOnce(t.Context()); firstDone <- err }()
	<-entered
	secondCtx, cancel := context.WithCancel(t.Context())
	secondDone := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); _, err := b.RunOnce(secondCtx); secondDone <- err }()
	<-started
	cancel()
	returned := false
	var secondErr error
	select {
	case secondErr = <-secondDone:
		returned = true
	case <-time.After(15 * time.Second):
	}
	close(release)
	require.NoError(t, <-firstDone)
	if !returned {
		secondErr = <-secondDone
	}
	assert.True(t, returned, "canceled admission must finish while the active pass is still held")
	require.ErrorIs(t, secondErr, context.Canceled)
}

func TestBridgeChangedLinkDuringReadSkipsPendingComment(t *testing.T) {
	for _, relink := range []bool{false, true} {
		t.Run(map[bool]string{false: "untrack", true: "same issue relink"}[relink], func(t *testing.T) {
			s, k, b, _, now := bridgeFixture(t)
			_, err := s.st.DB().Exec(s.st.Rebind("UPDATE open_loops SET status='closed',closed_reason='turn_flip',closed_by='system',closed_at=? WHERE id=1"), now)
			require.NoError(t, err)
			original := k.Service.Get
			k.get = func(ctx context.Context, ref string) (taskclient.KataTask, error) {
				issue, err := original(ctx, ref)
				if err != nil {
					return issue, err
				}
				link, err := s.st.GetLoopKataLinkContext(ctx, 1)
				require.NoError(t, err)
				require.NoError(t, s.Untrack(ctx, 1))
				if relink {
					_, _, err = s.st.InsertLoopKataLinkContext(ctx, link, "")
					require.NoError(t, err)
				}
				return issue, nil
			}
			comments := 0
			k.comment = func(context.Context, string, string, string) error { comments++; return nil }
			_, err = b.RunOnce(t.Context())
			require.NoError(t, err)
			assert.Zero(t, comments)
			row, err := s.st.GetOpenLoopContext(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, "closed", row.Status)
			if relink {
				require.NotNil(t, row.Kata)
				assert.Nil(t, row.Kata.ClosedNotifiedAt)
			} else {
				assert.Nil(t, row.Kata)
			}
		})
	}
}
