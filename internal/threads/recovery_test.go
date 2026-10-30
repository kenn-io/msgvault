package threads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func (f *sendFixture) setMedia(id, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mediaRaw == nil {
		f.mediaRaw = map[string]string{}
	}
	f.mediaRaw[id] = raw
}
func recoveryMediaJSON(id, body string, at time.Time) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":%q,"text":%q,"username":"synthetic_owner","media_type":"TEXT","permalink":"https://threads.example/post/synthetic","root_post":{"id":"100"},"replied_to":{"id":"100"},"is_reply":true,"is_reply_owned_by_me":true}`, id, at.UTC().Format(time.RFC3339Nano), body)
}
func disableRecoveryCredentials(t *testing.T, f *sendFixture) {
	t.Helper()
	f.options.LoadPolicy = func(context.Context, int64) (SendPolicy, error) {
		return SendPolicy{SourceIdentifier: writerSource}, nil
	}
	f.options.OpenAPI = func(context.Context, int64) (PublishAPI, []string, error) {
		require.Fail(t, "local recovery opened expired credentials")
		return nil, nil, ErrReauthorizationRequired
	}
}
func recoveryMessage(t *testing.T, f *sendFixture, id string) store.MessageMetadataRecord {
	t.Helper()
	record, found, err := f.st.ThreadsArchiveMessageMetadataContext(t.Context(), f.writer.sourceID, id)
	require.NoError(t, err)
	require.True(t, found)
	return record
}
func installArchiveFault(t *testing.T, st *store.Store) {
	t.Helper()
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_threads_archive() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic_archive_failure'; END $$;
 CREATE TRIGGER threads_archive_fault BEFORE INSERT ON messages FOR EACH ROW WHEN (NEW.source_message_id='500') EXECUTE FUNCTION fail_threads_archive()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER threads_archive_fault BEFORE INSERT ON messages WHEN NEW.source_message_id='500' BEGIN SELECT RAISE(FAIL,'synthetic_archive_failure'); END`)
	}
	require.NoError(t, err)
}
func dropArchiveFault(t *testing.T, st *store.Store) {
	t.Helper()
	query := `DROP TRIGGER threads_archive_fault`
	if st.IsPostgreSQL() {
		query += ` ON messages`
	}
	_, err := st.DB().ExecContext(t.Context(), query)
	require.NoError(t, err)
}
func TestThreadsSentArchivePendingReplay(t *testing.T) {
	for _, fault := range []string{"archive", "finish", "after-archive"} {
		t.Run(fault, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			sender := NewSender(f.st, f.options)
			switch fault {
			case "archive":
				installArchiveFault(t, f.st)
			case "finish":
				installSenderReceiptFault(t, f.st, "NEW.state='complete'")
			case "after-archive":
				sender.beforeArchiveFinish = func() error { return errors.New("synthetic archive interruption") }
			}
			result, err := sender.Send(t.Context(), f.request())
			require.Error(err)
			assert.Equal("sent_archive_pending", result.Status)
			assert.Equal("sent", result.RemoteStatus)
			assert.Equal("500", result.MediaID)
			assert.False(result.ArchiveComplete)
			r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Equal("500", r.MediaID)
			assert.Equal("sent_archive_pending", r.State)
			if fault == "archive" {
				dropArchiveFault(t, f.st)
			} else if fault == "finish" {
				dropSenderReceiptFault(t, f.st)
			}
			disableRecoveryCredentials(t, f)
			// A different handle replays the durable receipt and needs no publication
			// policy or credential refresh, including after the draft is consumed.
			reopened := concurrentSenderStore(t, f.st)
			result, err = NewSender(reopened, f.options).Send(t.Context(), f.request())
			require.NoError(err)
			assert.Equal("complete", result.Status)
			assert.True(result.ArchiveComplete)
			assert.Equal("500", result.MediaID)
			record := recoveryMessage(t, f, "500")
			assert.Equal(f.draft.Body, writerBody(t, f.st, record.ID))
			metadata := writerMetadata(t, f.st, record.ID)
			assert.Equal(true, metadata["sent_provisional"])
			assert.Equal(true, metadata["author_owned"])
			_, err = f.st.GetChatDraftContext(t.Context(), f.draft.DraftID)
			assert.ErrorIs(err, store.ErrChatDraftNotFound)
			again, err := NewSender(reopened, f.options).Send(t.Context(), f.request())
			require.NoError(err)
			assert.True(again.ArchiveComplete)
			bad := f.request()
			bad.Revision++
			_, err = NewSender(reopened, f.options).Send(t.Context(), bad)
			assert.ErrorIs(err, store.ErrChatDraftRevisionConflict)
			assert.Equal(int32(1), f.publishes.Load())
		})
	}
}
func TestThreadsSentArchiveKnownIDAfterCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sender := NewSender(f.st, f.options)
	sender.afterPublish = func(id string) { assert.Equal("500", id); cancel() }
	result, err := sender.Send(ctx, f.request())
	require.Error(err)
	assert.Equal("500", result.MediaID)
	assert.Equal("sent_archive_pending", result.Status)
	r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal("500", r.MediaID)
	disableRecoveryCredentials(t, f)
	result, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.True(result.ArchiveComplete)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsSentArchiveMediaIDPersistenceFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.setMedia("500", recoveryMediaJSON("500", f.draft.Body, writerTime))
	installSenderReceiptFault(t, f.st, "NEW.media_id IS NOT NULL")
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.ErrorIs(err, ErrReplyReceiptPersistence)
	assert.Equal("500", result.MediaID)
	assert.Equal("sent", result.RemoteStatus)
	assert.Equal("receipt_pending", result.LocalStatus)
	assert.Equal("receipt_persistence_failed", result.ErrorCategory)
	r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Empty(r.MediaID)
	assert.NotNil(r.PublishDispatchedAt)
	dropSenderReceiptFault(t, f.st)
	request := f.request()
	request.AdoptMediaID = "500"
	result, err = NewSender(f.st, f.options).Reconcile(t.Context(), request)
	require.NoError(err)
	assert.True(result.ArchiveComplete)
	assert.Equal("500", result.MediaID)
	assert.Equal(int32(1), f.publishes.Load())
}

func unknownFixture(t *testing.T) *sendFixture {
	t.Helper()
	f := newSendFixture(t)
	f.setMode("drop")
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.Error(t, err)
	require.Equal(t, "remote_unknown", result.Status)
	f.setMode("")
	return f
}
func TestThreadsUnknownExplicitAdoption(t *testing.T) {
	for _, kind := range []string{"lookup", "owner-false", "owner-missing", "owner-conflict", "root", "parent", "body", "text-missing", "not-reply", "revision", "unclaimed"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var f *sendFixture
			if kind == "unclaimed" {
				f = newSendFixture(t)
			} else {
				f = unknownFixture(t)
			}
			raw := recoveryMediaJSON("600", f.draft.Body, writerTime)
			switch kind {
			case "owner-false":
				raw = strings.Replace(raw, `"is_reply_owned_by_me":true`, `"is_reply_owned_by_me":false`, 1)
			case "owner-missing":
				raw = strings.Replace(raw, `,"is_reply_owned_by_me":true`, "", 1)
			case "owner-conflict":
				raw = strings.TrimSuffix(raw, "}") + `,"owner":{"id":"789"}}`
			case "root":
				raw = strings.Replace(raw, `"root_post":{"id":"100"}`, `"root_post":{"id":"200"}`, 1)
			case "parent":
				raw = strings.Replace(raw, `"replied_to":{"id":"100"}`, `"replied_to":{"id":"200"}`, 1)
			case "body":
				raw = strings.Replace(raw, f.draft.Body, "other reviewed body", 1)
			case "text-missing":
				raw = strings.Replace(raw, `"text":"`+f.draft.Body+`",`, "", 1)
			case "not-reply":
				raw = strings.Replace(raw, `"is_reply":true`, `"is_reply":false`, 1)
			}
			if kind != "lookup" {
				f.setMedia("600", raw)
			}
			request := f.request()
			request.AdoptMediaID = "600"
			if kind == "revision" {
				request.Revision++
			}
			_, err := NewSender(f.st, f.options).Reconcile(t.Context(), request)
			require.Error(err)
			if kind != "unclaimed" {
				receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
				require.NoError(err)
				assert.Empty(receipt.MediaID)
				assert.False(receipt.ArchiveComplete)
				assert.Equal(int32(1), f.publishes.Load())
			} else {
				assert.Zero(f.publishes.Load())
				assert.Zero(f.opens.Load())
			}
		})
	}
	t.Run("explicit own ID with read scopes after optout", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := unknownFixture(t)
		f.setMedia("600", recoveryMediaJSON("600", f.draft.Body, writerTime))
		f.options.LoadPolicy = func(context.Context, int64) (SendPolicy, error) {
			return SendPolicy{SourceIdentifier: writerSource}, nil
		}
		f.options.OpenAPI = func(context.Context, int64) (PublishAPI, []string, error) {
			return f.api, []string{"threads_basic", "threads_read_replies"}, nil
		}
		request := f.request()
		request.AdoptMediaID = "600"
		result, err := NewSender(f.st, f.options).Reconcile(t.Context(), request)
		require.NoError(err)
		assert.Equal("600", result.MediaID)
		assert.True(result.ArchiveComplete)
		assert.Equal(int32(1), f.publishes.Load())
		disableRecoveryCredentials(t, f)
		result, err = NewSender(f.st, f.options).Send(t.Context(), request)
		require.NoError(err)
		assert.True(result.ArchiveComplete)
		request.AdoptMediaID = "601"
		_, err = NewSender(f.st, f.options).Send(t.Context(), request)
		require.Error(err)
	})
}
func TestThreadsUnknownCandidatesAreBoundedHints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := unknownFixture(t)
	receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	require.NotNil(receipt.PublishDispatchedAt)
	f.clock.advance(time.Now().UTC().Add(time.Hour).Sub(f.clock.Now()))
	f.api, err = NewClient(ClientOptions{BaseURL: f.api.base.String(), HTTPClient: f.api.http, AccessToken: testToken, Budget: f.clock.budget(1000)})
	require.NoError(err)
	for n := range 150 {
		f.recoveryReplies = append(f.recoveryReplies, recoveryMediaJSON(fmt.Sprint(600+n), f.draft.Body, *receipt.PublishDispatchedAt))
	}
	result, err := NewSender(f.st, f.options).Reconcile(t.Context(), f.request())
	require.NoError(err)
	assert.Equal("remote_unknown", result.Status)
	assert.Empty(result.MediaID)
	require.Len(result.Candidates, 100)
	assert.Equal("600", result.Candidates[0])
	assert.Equal("699", result.Candidates[99])
	assert.Equal(1, f.recoveryListCalls)
	receipt, err = f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Empty(receipt.MediaID)
	// Both legitimate identical replies may be archived without being adopted.
	for _, id := range []string{"600", "601"} {
		m := writerReply(id, "100", "100")
		m.Text = &f.draft.Body
		applyWriter(t, f.writer, m, ItemOrigin{Kind: "own_replies"})
	}
	again, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Empty(again.MediaID)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsSyncSendSnapshotOrder(t *testing.T) {
	for _, order := range []string{"canonical-first", "provisional-first"} {
		t.Run(order, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			if order == "canonical-first" {
				installArchiveFault(t, f.st)
				_, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.Error(err)
				dropArchiveFault(t, f.st)
			} else {
				result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.NoError(err)
				require.True(result.ArchiveComplete)
			}
			canonical := writerReply("500", "100", "100")
			canonical.Timestamp = writerTime.Add(time.Hour)
			canonical.Username = new("synthetic_owner")
			canonical.Permalink = new("https://threads.example/post/synthetic")
			canonical.Text = new("first canonical")
			id := applyWriter(t, f.writer, canonical, ItemOrigin{Kind: "own_replies"})
			canonical.Text = new("canonical edited")
			applyWriter(t, f.writer, canonical, ItemOrigin{Kind: "own_replies"})
			disableRecoveryCredentials(t, f)
			result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.NoError(err)
			assert.True(result.ArchiveComplete)
			assert.Equal("canonical edited", writerBody(t, f.st, id))
			metadata := writerMetadata(t, f.st, id)
			assert.Equal(false, metadata["sent_provisional"])
			assert.Equal(true, metadata["text_changed"])
			assert.Equal("synthetic_owner", metadata["first_username"])
			assert.Equal("https://threads.example/post/synthetic", metadata["permalink"])
			receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Equal(f.draft.Body, receipt.Body)
			var count int
			require.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id='500'`), f.writer.sourceID).Scan(&count))
			assert.Equal(1, count)
			assert.Equal(int32(1), f.publishes.Load())
		})
	}
}
func TestThreadsSentArchiveUsesAvailableCanonicalSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.setMedia("500", recoveryMediaJSON("500", f.draft.Body, writerTime.Add(time.Hour)))
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	require.True(result.ArchiveComplete)
	record := recoveryMessage(t, f, "500")
	metadata := writerMetadata(t, f.st, record.ID)
	assert.Equal(false, metadata["sent_provisional"])
	assert.Equal("synthetic_owner", metadata["current_username"])
	assert.Equal(writerTime.Add(time.Hour).Format(time.RFC3339Nano), metadata["timestamp"])
}

// Hegel reduced recovery to completion position0/replays0/owner1/body="r".
func TestThreadsRecoveryArchiveFailureRegression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	draft, err := f.st.CreateChatDraftContext(t.Context(), f.draft.ConversationID, f.rootID, "r", func(string, string) error { return nil })
	require.NoError(err)
	f.draft = draft
	installArchiveFault(t, f.st)
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.Error(err)
	assert.Equal("500", result.MediaID)
	assert.Equal("sent_archive_pending", result.Status)
	dropArchiveFault(t, f.st)
	disableRecoveryCredentials(t, f)
	result, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	require.True(result.ArchiveComplete)
	m := writerReply("500", "100", "100")
	m.Timestamp = f.clock.Now().Add(-time.Hour)
	m.Username = new("synthetic_owner_1")
	m.Permalink = new("https://threads.example/post/synthetic")
	m.Text = new("first-r")
	id := applyWriter(t, f.writer, m, ItemOrigin{Kind: "own_replies"})
	m.Text = new("edited-r")
	applyWriter(t, f.writer, m, ItemOrigin{Kind: "own_replies"})
	assert.Equal("edited-r", writerBody(t, f.st, id))
	assert.Equal(int32(1), f.publishes.Load())
}

func TestThreadsSentArchiveCanonicalCredentialFailureRetainsSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.setMedia("500", `{"error":{"code":190}}`)
	f.mediaStatus = map[string]int{"500": 400}
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	apiErr, ok := errors.AsType[*APIError](err)
	require.True(ok, "%v", err)
	assert.True(apiErr.CredentialInvalid())
	assert.True(result.ArchiveComplete)
	assert.Equal("complete", result.Status)
	assert.Equal("500", result.MediaID)
	assert.Equal("reauthorization_required", result.ErrorCategory)
	disableRecoveryCredentials(t, f)
	result, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.True(result.ArchiveComplete)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsSentArchiveCanonicalConflictRemainsPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	raw := strings.Replace(recoveryMediaJSON("500", f.draft.Body, writerTime), `"root_post":{"id":"100"}`, `"root_post":{"id":"200"}`, 1)
	f.setMedia("500", raw)
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.Error(err)
	assert.Equal("sent_archive_pending", result.Status)
	assert.Equal("500", result.MediaID)
	assert.False(result.ArchiveComplete)
	receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal("500", receipt.MediaID)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsUnknownOwnerAdmissionBeforeReceipt(t *testing.T) {
	assert := assert.New(t)
	denied := errors.New("owner only")
	sender := NewSender(nil, SenderOptions{Authorize: func(context.Context) error { return denied }})
	result, err := sender.Reconcile(t.Context(), SendRequest{DraftID: "chat-draft-private", Revision: 1, AdoptMediaID: "500"})
	assert.ErrorIs(err, denied)
	assert.Empty(result.MediaID)
	assert.Zero(result.BodyBytes)
	assert.Empty(result.Candidates)
}
