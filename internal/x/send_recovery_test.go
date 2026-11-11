package x

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func sendReadOnly(t *testing.T, f *sendFixture) {
	t.Helper()
	f.policy.AllowSend = false
	record, err := f.manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	record.GrantedScopes = []string{"tweet.read", "users.read"}
	require.NoError(t, f.manager.Save(t.Context(), record))
}

func TestXSendExplicitAdoptionOnlyForMatchingFrozenAttempt(t *testing.T) {
	for _, mode := range []string{"matching", "wrong_author", "wrong_parent", "missing_parent", "omitted_item", "stale_revision", "no_attempt", "rejected", "same_accepted_id", "different_accepted_id"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newSendFixture(t)
			var undo func()
			switch mode {
			case "same_accepted_id", "different_accepted_id":
				undo = sendFailure(t, f.st, "message_bodies", "TRUE")
			case "rejected":
				f.post = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprint(w, `{"title":"Rejected"}`)
				}
			default:
				f.post = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprint(w, `{"title":"Unknown"}`)
				}
			}
			if mode != "no_attempt" {
				_, err := f.sender().Send(t.Context(), f.request())
				_testRequire.Error(err)
			}
			if undo != nil {
				undo()
			}
			before := f.posts.Load()
			sendReadOnly(t, f)
			// A changed current create estimate cannot reprice the old reservation.
			f.policy.CreateReservationMicros = 900000
			req := f.request()
			req.AdoptPost = "9001"
			switch mode {
			case "stale_revision":
				req.Revision++
			case "different_accepted_id":
				req.AdoptPost = "9002"
			case "wrong_author", "wrong_parent", "missing_parent", "omitted_item":
				f.get = func(w http.ResponseWriter, _ *http.Request) {
					if mode == "omitted_item" {
						_, _ = fmt.Fprint(w, `{"data":[]}`)
						return
					}
					author, parent := "1001", "102"
					if mode == "wrong_author" {
						author = "1002"
					}
					if mode == "wrong_parent" {
						parent = "202"
					}
					refs := fmt.Sprintf(`,"referenced_tweets":[{"type":"replied_to","id":%q}]`, parent)
					if mode == "missing_parent" {
						refs = ""
					}
					_, _ = fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":%q,"conversation_id":"100","text":%q,"created_at":"2026-10-31T12:00:00Z","edit_history_tweet_ids":["9001"]%s}]}`, author, f.draft.Body, refs)
				}
			}
			p, prepareErr := f.sender().PrepareReply(t.Context(), req)
			result, sendErr := f.sender().Send(t.Context(), req)
			switch mode {
			case "matching":
				_testRequire.NoError(prepareErr)
				_testRequire.NoError(sendErr)
				_testAssert.Equal("adopt", p.Mode)
				_testAssert.Equal(f.draft.Body, p.Text)
				_testAssert.Equal("provider reply", p.AdoptText)
				_testAssert.Equal("1001", p.AdoptAuthorID)
				_testAssert.Equal("102", p.TargetPostID)
				_testAssert.Equal("9001", p.AdoptPost)
				_testAssert.Equal(int64(200000), p.ReservedMicros)
				_testAssert.Equal("sent", result.Status)
				_testAssert.Equal("9001", result.RemoteID)
				_testAssert.Equal("owner_confirmed", result.Association)
				_testAssert.Equal(int32(2), f.gets.Load())
				a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
				_testRequire.NoError(err)
				var owner, id string
				var confirmed bool
				_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT adoption_owner_id,adopted_post_id,adoption_confirmed_at IS NOT NULL FROM x_reply_attempts WHERE source_id=? AND draft_id=? AND attempt_id=?`), a.SourceID, a.DraftID, a.AttemptID).Scan(&owner, &id, &confirmed))
				_testAssert.Equal("1001", owner)
				_testAssert.Equal("9001", id)
				_testAssert.True(confirmed)
				state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
				_testRequire.NoError(err)
				_testAssert.Zero(state.SpendBuckets[0].ReservedMicros)
				_testAssert.Equal(int64(230000), state.SpendBuckets[0].SpentMicros)
				target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
				_testRequire.NoError(err)
				body, err := f.st.GetMessageBodyText(t.Context(), target.MessageID)
				_testRequire.NoError(err)
				_testAssert.Equal("provider reply", body)
				again, err := f.sender().Send(t.Context(), req)
				_testRequire.NoError(err)
				_testAssert.Equal(result, again)
				_testAssert.Equal(int32(2), f.gets.Load())
			case "same_accepted_id":
				_testRequire.NoError(prepareErr)
				_testRequire.NoError(sendErr)
				_testAssert.Equal("repair", p.Mode)
				_testAssert.Equal("sent", result.Status)
				_testAssert.Equal("9001", result.RemoteID)
				_testAssert.Empty(result.Association)
				_testAssert.Zero(f.gets.Load())
			default:
				_testRequire.Error(prepareErr)
				_testRequire.Error(sendErr)
				if mode == "wrong_author" || mode == "wrong_parent" || mode == "missing_parent" || mode == "omitted_item" {
					_testAssert.Equal(int32(2), f.gets.Load())
				} else {
					_testAssert.Zero(f.gets.Load())
				}
			}
			_testAssert.Equal(before, f.posts.Load(), "adoption never POSTs")
		})
	}
}

func TestXSendKnownIDPersistenceFailureRequiresExplicitAdoption(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newSendFixture(t)
	undo := sendFailure(t, f.st, "x_reply_attempts", "NEW.state = 'remote_accepted'")
	result, err := f.sender().Send(t.Context(), f.request())
	_testRequire.Error(err)
	_testAssert.Equal("sent_archive_pending", result.Status)
	_testAssert.Equal("9001", result.RemoteID)
	_testAssert.Equal("acceptance_write_failed", result.Code)
	a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal("admitted", a.State)
	_testAssert.Empty(a.RemoteID)
	undo()
	f.st = reopenSendStore(t, f.st)
	again, err := f.sender().Send(t.Context(), f.request())
	_testRequire.NoError(err)
	_testAssert.Equal("remote_unknown", again.Status)
	_testAssert.Empty(again.RemoteID)
	sendReadOnly(t, f)
	req := f.request()
	req.AdoptPost = "9001"
	result, err = f.sender().Send(t.Context(), req)
	_testRequire.NoError(err)
	_testAssert.Equal("9001", result.RemoteID)
	_testAssert.Equal("sent", result.Status)
	_testAssert.Equal(int32(1), f.posts.Load())
	_testAssert.Equal(int32(1), f.gets.Load())
}

type sendRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn sendRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type sendCancelBody struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (b sendCancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.cancel()
	}
	return n, err
}

func TestXSendValidSuccessAfterCancellationPersistsReceipt(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newSendFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	credentials := f.manager.Credentials("1001", f.server.Client())
	client := *credentials.HTTPClient()
	base := client.Transport
	client.Transport = sendRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && resp.StatusCode == http.StatusCreated {
			resp.Body = sendCancelBody{ReadCloser: resp.Body, cancel: cancel}
		}
		return resp, err
	})
	sender := NewSender(f.st, NewClient(f.server.URL, &client), f.policy, credentials)
	result, err := sender.Send(ctx, f.request())
	_testRequire.NoError(err)
	_testRequire.ErrorIs(ctx.Err(), context.Canceled)
	_testAssert.Equal("sent", result.Status)
	_testAssert.Equal("9001", result.RemoteID)
	a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal("sent", a.State)
	again, err := f.sender().Send(t.Context(), f.request())
	_testRequire.NoError(err)
	_testAssert.Equal(result, again)
	_testAssert.Equal(int32(1), f.posts.Load())
}

func TestXSendSyncRemovalShareFence(t *testing.T) {
	_testRequire := require.New(t)

	f := newSendFixture(t)
	peer := openSendStore(t, f.st, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	f.post = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"data":{"id":"9001"}}`)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.sender().Send(ctx, f.request()); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		_testRequire.NoError(ctx.Err())
	}
	// This is the real credential lifecycle file used by a separate manager.
	// Inspect its nonblocking OS lock while the provider request is held.
	guard := flock.New(filepath.Join(f.manager.dir, ".x-lifecycle-1001.lock"))
	locked, err := guard.TryLock()
	_testRequire.NoError(err)
	if locked {
		_testRequire.NoError(guard.Unlock())
	}
	assert.False(t, locked, "reauth credential lifecycle must wait for the send")
	_, err = peer.AcquireSyncExecutionContext(ctx, f.source.ID)
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	_, err = peer.AcquireSourceRemovalLockContext(ctx, f.source.ID)
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	finish()
	select {
	case err = <-done:
		_testRequire.NoError(err)
	case <-ctx.Done():
		_testRequire.NoError(ctx.Err())
	}
	locked, err = guard.TryLock()
	_testRequire.NoError(err)
	_testRequire.True(locked)
	_testRequire.NoError(guard.Unlock())
}

func TestXAcceptedRepairHonorsSuppression(t *testing.T) {
	for _, mode := range []string{"accepted_removed", "parent_removed", "parent_edited"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newSendFixture(t)
			undo := sendFailure(t, f.st, "message_bodies", "TRUE")
			result, err := f.sender().Send(t.Context(), f.request())
			_testRequire.Error(err)
			_testAssert.Equal("9001", result.RemoteID)
			undo()
			switch mode {
			case "accepted_removed":
				_, err = f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "9001", "removed")
			case "parent_removed":
				_, err = f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "102", "removed")
			case "parent_edited":
				now := time.Now()
				_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "103", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102", "103"}, Text: "edited parent", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
			}
			_testRequire.NoError(err)
			sendReadOnly(t, f)
			result, err = f.sender().Send(t.Context(), f.request())
			_testRequire.NoError(err)
			_testAssert.Equal("sent", result.Status)
			target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
			_testRequire.NoError(err)
			if mode == "accepted_removed" {
				_testAssert.Equal("removed", target.Availability)
				_testAssert.Zero(target.MessageID)
			} else {
				m, err := f.st.GetMessageMetadataContext(t.Context(), target.MessageID)
				_testRequire.NoError(err)
				var metadata struct {
					Parent string `json:"referenced_message_id"`
				}
				_testRequire.NoError(json.Unmarshal([]byte(m.String), &metadata))
				_testAssert.Equal("102", metadata.Parent)
			}
			_testAssert.Equal(int32(1), f.posts.Load())
			_testAssert.Zero(f.gets.Load())
		})
	}
}

func TestXAcceptedRepairPreservesProviderSnapshot(t *testing.T) {
	for _, mode := range []string{"richer", "wrong_author", "wrong_parent"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newSendFixture(t)
			undo := sendFailure(t, f.st, "message_bodies", "TRUE")
			_, err := f.sender().Send(t.Context(), f.request())
			_testRequire.Error(err)
			undo()
			author, parent := "1001", "102"
			if mode == "wrong_author" {
				author = "1002"
			}
			if mode == "wrong_parent" {
				parent = "202"
			}
			now := time.Now()
			page := Page{Posts: []Post{{ID: "9001", AuthorID: author, ConversationID: "100", Text: "classic", LongText: "authoritative full provider text", CreatedAt: now, References: []Reference{{Type: "replied_to", ID: parent}}, EditHistory: []string{"9001"}}}, Users: []User{{ID: author, Username: "example_provider", ProtectedKnown: true}}, FetchedAt: now, PostCount: 1, UserCount: 1}
			_testRequire.NoError(WritePage(t.Context(), f.st, f.source.ID, "1001", page, now))
			target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
			_testRequire.NoError(err)
			before, err := f.st.GetMessageContext(t.Context(), target.MessageID)
			_testRequire.NoError(err)
			result, err := f.sender().Send(t.Context(), f.request())
			if mode == "richer" {
				_testRequire.NoError(err)
				_testAssert.Equal("sent", result.Status)
			} else {
				_testRequire.Error(err)
				_testAssert.Equal("sent_archive_pending", result.Status)
			}
			after, err := f.st.GetMessageContext(t.Context(), target.MessageID)
			_testRequire.NoError(err)
			_testAssert.Equal(before, after)
			body, err := f.st.GetMessageBodyText(t.Context(), target.MessageID)
			_testRequire.NoError(err)
			_testAssert.Equal("authoritative full provider text", body)
			_testAssert.Equal(int32(1), f.posts.Load())
		})
	}
}

func TestXSendProviderReplacesEstimatedSnapshot(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newSendFixture(t)
	_, err := f.sender().Send(t.Context(), f.request())
	_testRequire.NoError(err)
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	_testRequire.NoError(err)
	before, err := f.st.GetMessageContext(t.Context(), target.MessageID)
	_testRequire.NoError(err)
	var metadata struct {
		Estimated bool `json:"estimated_timestamp"`
	}
	raw, err := f.st.GetMessageMetadataContext(t.Context(), target.MessageID)
	_testRequire.NoError(err)
	_testRequire.NoError(json.Unmarshal([]byte(raw.String), &metadata))
	_testRequire.True(metadata.Estimated)
	providerAt := before.SentAt.Add(time.Hour)
	fetchedAt := providerAt.Add(time.Hour)
	page := Page{Posts: []Post{{ID: "9001", AuthorID: "1001", ConversationID: "100", Text: "provider normalized reply", TextIncomplete: true, CreatedAt: providerAt, References: []Reference{{Type: "replied_to", ID: "102"}}, EditHistory: []string{"9001"}}}, Users: []User{{ID: "1001", Username: "example_owner", ProtectedKnown: true}}, FetchedAt: fetchedAt, PostCount: 1, UserCount: 1}
	_testRequire.NoError(WritePage(t.Context(), f.st, f.source.ID, "1001", page, fetchedAt))
	same, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	_testRequire.NoError(err)
	_testAssert.Equal(target.MessageID, same.MessageID)
	after, err := f.st.GetMessageContext(t.Context(), same.MessageID)
	_testRequire.NoError(err)
	raw, err = f.st.GetMessageMetadataContext(t.Context(), same.MessageID)
	_testRequire.NoError(err)
	_testRequire.NoError(json.Unmarshal([]byte(raw.String), &metadata))
	_testAssert.False(metadata.Estimated)
	_testAssert.True(providerAt.Equal(after.SentAt), "actual provider timestamp must replace estimate")
	body, err := f.st.GetMessageBodyText(t.Context(), same.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("provider normalized reply", body)
}
