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
			f := newSendFixture(t)
			var undo func()
			switch mode {
			case "same_accepted_id", "different_accepted_id":
				undo = sendFailure(t, f.st, "message_bodies", "TRUE")
			case "rejected":
				f.post = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(403)
					fmt.Fprint(w, `{"title":"Rejected"}`)
				}
			default:
				f.post = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503); fmt.Fprint(w, `{"title":"Unknown"}`) }
			}
			if mode != "no_attempt" {
				_, err := f.sender().Send(t.Context(), f.request())
				require.Error(t, err)
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
						fmt.Fprint(w, `{"data":[]}`)
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
					fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":%q,"conversation_id":"100","text":%q,"created_at":"2026-10-31T12:00:00Z","edit_history_tweet_ids":["9001"]%s}]}`, author, f.draft.Body, refs)
				}
			}
			p, prepareErr := f.sender().PrepareReply(t.Context(), req)
			result, sendErr := f.sender().Send(t.Context(), req)
			if mode == "matching" {
				require.NoError(t, prepareErr)
				require.NoError(t, sendErr)
				assert.Equal(t, "adopt", p.Mode)
				assert.Equal(t, f.draft.Body, p.Text)
				assert.Equal(t, "provider reply", p.AdoptText)
				assert.Equal(t, "1001", p.AdoptAuthorID)
				assert.Equal(t, "102", p.TargetPostID)
				assert.Equal(t, "9001", p.AdoptPost)
				assert.Equal(t, int64(200000), p.ReservedMicros)
				assert.Equal(t, "sent", result.Status)
				assert.Equal(t, "9001", result.RemoteID)
				assert.Equal(t, "owner_confirmed", result.Association)
				assert.Equal(t, int32(2), f.gets.Load())
				a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
				require.NoError(t, err)
				var owner, id string
				var confirmed bool
				require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT adoption_owner_id,adopted_post_id,adoption_confirmed_at IS NOT NULL FROM x_reply_attempts WHERE source_id=? AND draft_id=? AND attempt_id=?`), a.SourceID, a.DraftID, a.AttemptID).Scan(&owner, &id, &confirmed))
				assert.Equal(t, "1001", owner)
				assert.Equal(t, "9001", id)
				assert.True(t, confirmed)
				state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
				require.NoError(t, err)
				assert.Zero(t, state.SpendBuckets[0].ReservedMicros)
				assert.Equal(t, int64(230000), state.SpendBuckets[0].SpentMicros)
				target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
				require.NoError(t, err)
				body, err := f.st.GetMessageBodyText(target.MessageID)
				require.NoError(t, err)
				assert.Equal(t, "provider reply", body)
				again, err := f.sender().Send(t.Context(), req)
				require.NoError(t, err)
				assert.Equal(t, result, again)
				assert.Equal(t, int32(2), f.gets.Load())
			} else if mode == "same_accepted_id" {
				require.NoError(t, prepareErr)
				require.NoError(t, sendErr)
				assert.Equal(t, "repair", p.Mode)
				assert.Equal(t, "sent", result.Status)
				assert.Equal(t, "9001", result.RemoteID)
				assert.Empty(t, result.Association)
				assert.Zero(t, f.gets.Load())
			} else {
				require.Error(t, prepareErr)
				require.Error(t, sendErr)
				if mode == "wrong_author" || mode == "wrong_parent" || mode == "missing_parent" || mode == "omitted_item" {
					assert.Equal(t, int32(2), f.gets.Load())
				} else {
					assert.Zero(t, f.gets.Load())
				}
			}
			assert.Equal(t, before, f.posts.Load(), "adoption never POSTs")
		})
	}
}

func TestXSendKnownIDPersistenceFailureRequiresExplicitAdoption(t *testing.T) {
	f := newSendFixture(t)
	undo := sendFailure(t, f.st, "x_reply_attempts", "NEW.state = 'remote_accepted'")
	result, err := f.sender().Send(t.Context(), f.request())
	require.Error(t, err)
	assert.Equal(t, "sent_archive_pending", result.Status)
	assert.Equal(t, "9001", result.RemoteID)
	assert.Equal(t, "acceptance_write_failed", result.Code)
	a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "admitted", a.State)
	assert.Empty(t, a.RemoteID)
	undo()
	f.st = reopenSendStore(t, f.st)
	again, err := f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "remote_unknown", again.Status)
	assert.Empty(t, again.RemoteID)
	sendReadOnly(t, f)
	req := f.request()
	req.AdoptPost = "9001"
	result, err = f.sender().Send(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, "9001", result.RemoteID)
	assert.Equal(t, "sent", result.Status)
	assert.Equal(t, int32(1), f.posts.Load())
	assert.Equal(t, int32(1), f.gets.Load())
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
	f := newSendFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	credentials := f.manager.Credentials("1001", f.server.Client())
	client := *credentials.HTTPClient()
	base := client.Transport
	client.Transport = sendRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && resp.StatusCode == 201 {
			resp.Body = sendCancelBody{ReadCloser: resp.Body, cancel: cancel}
		}
		return resp, err
	})
	sender := NewSender(f.st, NewClient(f.server.URL, &client), f.policy, credentials)
	result, err := sender.Send(ctx, f.request())
	require.NoError(t, err)
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Equal(t, "sent", result.Status)
	assert.Equal(t, "9001", result.RemoteID)
	a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "sent", a.State)
	again, err := f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, result, again)
	assert.Equal(t, int32(1), f.posts.Load())
}

func TestXSendSyncRemovalShareFence(t *testing.T) {
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
		w.WriteHeader(201)
		fmt.Fprint(w, `{"data":{"id":"9001"}}`)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.sender().Send(ctx, f.request()); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	// This is the real credential lifecycle file used by a separate manager.
	// Inspect its nonblocking OS lock while the provider request is held.
	guard := flock.New(filepath.Join(f.manager.dir, ".x-lifecycle-1001.lock"))
	locked, err := guard.TryLock()
	require.NoError(t, err)
	if locked {
		require.NoError(t, guard.Unlock())
	}
	assert.False(t, locked, "reauth credential lifecycle must wait for the send")
	_, err = peer.AcquireSyncExecutionContext(ctx, f.source.ID)
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	_, err = peer.AcquireSourceRemovalLockContext(ctx, f.source.ID)
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	finish()
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	locked, err = guard.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, guard.Unlock())
}

func TestXAcceptedRepairHonorsSuppression(t *testing.T) {
	for _, mode := range []string{"accepted_removed", "parent_removed", "parent_edited"} {
		t.Run(mode, func(t *testing.T) {
			f := newSendFixture(t)
			undo := sendFailure(t, f.st, "message_bodies", "TRUE")
			result, err := f.sender().Send(t.Context(), f.request())
			require.Error(t, err)
			assert.Equal(t, "9001", result.RemoteID)
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
			require.NoError(t, err)
			sendReadOnly(t, f)
			result, err = f.sender().Send(t.Context(), f.request())
			require.NoError(t, err)
			assert.Equal(t, "sent", result.Status)
			target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
			require.NoError(t, err)
			if mode == "accepted_removed" {
				assert.Equal(t, "removed", target.Availability)
				assert.Zero(t, target.MessageID)
			} else {
				m, err := f.st.GetMessageMetadataContext(t.Context(), target.MessageID)
				require.NoError(t, err)
				var metadata struct {
					Parent string `json:"referenced_message_id"`
				}
				require.NoError(t, json.Unmarshal([]byte(m.String), &metadata))
				assert.Equal(t, "102", metadata.Parent)
			}
			assert.Equal(t, int32(1), f.posts.Load())
			assert.Zero(t, f.gets.Load())
		})
	}
}

func TestXAcceptedRepairPreservesProviderSnapshot(t *testing.T) {
	for _, mode := range []string{"richer", "wrong_author", "wrong_parent"} {
		t.Run(mode, func(t *testing.T) {
			f := newSendFixture(t)
			undo := sendFailure(t, f.st, "message_bodies", "TRUE")
			_, err := f.sender().Send(t.Context(), f.request())
			require.Error(t, err)
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
			require.NoError(t, WritePage(t.Context(), f.st, f.source.ID, "1001", page, now))
			target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
			require.NoError(t, err)
			before, err := f.st.GetMessageContext(t.Context(), target.MessageID)
			require.NoError(t, err)
			result, err := f.sender().Send(t.Context(), f.request())
			if mode == "richer" {
				require.NoError(t, err)
				assert.Equal(t, "sent", result.Status)
			} else {
				require.Error(t, err)
				assert.Equal(t, "sent_archive_pending", result.Status)
			}
			after, err := f.st.GetMessageContext(t.Context(), target.MessageID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			body, err := f.st.GetMessageBodyText(target.MessageID)
			require.NoError(t, err)
			assert.Equal(t, "authoritative full provider text", body)
			assert.Equal(t, int32(1), f.posts.Load())
		})
	}
}

func TestXSendProviderReplacesEstimatedSnapshot(t *testing.T) {
	f := newSendFixture(t)
	_, err := f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	require.NoError(t, err)
	before, err := f.st.GetMessageContext(t.Context(), target.MessageID)
	require.NoError(t, err)
	var metadata struct {
		Estimated bool `json:"estimated_timestamp"`
	}
	raw, err := f.st.GetMessageMetadataContext(t.Context(), target.MessageID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw.String), &metadata))
	require.True(t, metadata.Estimated)
	providerAt := before.SentAt.Add(time.Hour)
	fetchedAt := providerAt.Add(time.Hour)
	page := Page{Posts: []Post{{ID: "9001", AuthorID: "1001", ConversationID: "100", Text: "provider normalized reply", TextIncomplete: true, CreatedAt: providerAt, References: []Reference{{Type: "replied_to", ID: "102"}}, EditHistory: []string{"9001"}}}, Users: []User{{ID: "1001", Username: "example_owner", ProtectedKnown: true}}, FetchedAt: fetchedAt, PostCount: 1, UserCount: 1}
	require.NoError(t, WritePage(t.Context(), f.st, f.source.ID, "1001", page, fetchedAt))
	same, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	require.NoError(t, err)
	assert.Equal(t, target.MessageID, same.MessageID)
	after, err := f.st.GetMessageContext(t.Context(), same.MessageID)
	require.NoError(t, err)
	raw, err = f.st.GetMessageMetadataContext(t.Context(), same.MessageID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw.String), &metadata))
	assert.False(t, metadata.Estimated)
	assert.True(t, providerAt.Equal(after.SentAt), "actual provider timestamp must replace estimate")
	body, err := f.st.GetMessageBodyText(same.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "provider normalized reply", body)
}
