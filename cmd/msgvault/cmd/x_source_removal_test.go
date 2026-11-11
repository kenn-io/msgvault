package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

func TestXSourceRemovalDropsCredentialsAfterCascadeDespiteRevokeFailure(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	stage := f.deps.manager(xStagedTokensDir(f.cfg))
	_testRequire.NoError(stage.Save(t.Context(), xTestRecord("staged-removal-access")))
	var revokes atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revokes.Add(1)
		_, err := f.st.GetSourceByID(f.source.ID)
		assert.ErrorIs(t, err, store.ErrSourceNotFound, "revocation must follow committed cascade")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)
	d := f.deps
	d.revokeURL = provider.URL
	_, _, err := removeXSource(t.Context(), f.st, f.cfg, f.source, d, false)
	_testRequire.NoError(err, "failed remote revoke must not retain local credentials")
	_testAssert.Positive(revokes.Load())
	_, err = f.deps.manager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	_testRequire.ErrorIs(err, os.ErrNotExist)
	_, err = stage.Load(t.Context(), "1001")
	_testRequire.ErrorIs(err, os.ErrNotExist)
}

func TestXSourceRemovalWaitsForCrossSourceContentCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	stage := f.deps.manager(xStagedTokensDir(f.cfg))
	_testRequire.NoError(stage.Save(t.Context(), xTestRecord("pending-cleanup-access")))

	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	copySource, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	copyPost := store.XPersistPostInput{
		SourceID: copySource.ID, OwnerID: "2001", PostID: "900", AuthorID: "3001",
		ConversationID: "900", EditHistory: []string{"900"}, Text: "surviving source copy",
		CreatedAt: at.Add(-time.Minute), FetchedAt: at, Availability: "public",
		Author: &store.XAuthorInput{SourceID: copySource.ID, UserID: "3001", Username: "example_author", DisplayName: "Example Author", FetchedAt: at},
	}
	saved, err := f.st.PersistXPostContext(t.Context(), copyPost)
	_testRequire.NoError(err)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	released := false
	defer func() {
		if !released {
			assert.NoError(t, execution.Release())
		}
	}()
	work, err := f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "900", "withheld")
	_testRequire.NoError(err)
	_testAssert.True(work.Pending)
	_testRequire.NoError(execution.Release())
	released = true

	var revokes atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revokes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(provider.Close)
	d := f.deps
	d.revokeURL = provider.URL
	_, _, err = removeXSource(t.Context(), f.st, f.cfg, f.source, d, false)
	_testRequire.Error(err)
	_testAssert.Contains(err.Error(), "source x sync")
	_testAssert.Contains(err.Error(), "hint:")
	_testAssert.Zero(revokes.Load(), "credentials remain usable until derived cleanup completes")
	_, err = f.st.GetSourceByID(f.source.ID)
	_testRequire.NoError(err)
	_, err = stage.Load(t.Context(), f.source.Identifier)
	_testRequire.NoError(err)
	body, err := f.st.GetMessageBodyText(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("surviving source copy", body)
	var pending int
	_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?`), f.source.ID, "900").Scan(&pending))
	_testAssert.Equal(1, pending)

	copyPost.FetchedAt = copyPost.FetchedAt.Add(time.Minute)
	copyPost.Author.FetchedAt = copyPost.FetchedAt
	result, err := f.st.PersistXPostContext(t.Context(), copyPost)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped, "the retained pending alias must continue suppressing copies from other X sources")
}

func TestXRemoveRacesWithSync(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	f.entered = make(chan struct{})
	finished := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		_, err := importXSourceWithDeps(ctx, f.st, f.cfg, f.source, xapi.ImportOptions{}, f.deps)
		finished <- err
	}()
	<-f.entered // Actual mentions HTTP owns this source until cancellation.
	_, _, err := removeXSource(t.Context(), f.st, f.cfg, f.source, f.deps, false)
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	_, err = f.st.GetSourceByID(f.source.ID)
	_testRequire.NoError(err)
	_, err = f.deps.manager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	_testRequire.NoError(err)
	cancel()
	_testRequire.ErrorIs(<-finished, context.Canceled)
	// A durable uncertain receipt is a cascade obligation, not permission to
	// resend. Seed the real schema so deleting zero receipt rows is no proof.
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "101")
	_testRequire.NoError(err)
	var conversation int64
	_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), target.MessageID).Scan(&conversation))
	_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), "removal-draft", f.source.ID, conversation, "101", "channel", "101", "synthetic reviewed body", 1)
	_testRequire.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), f.source.ID, "1001", "removal-draft", "attempt-1", 1, "101", "101", "synthetic-digest", "remote_unknown", "reservation-1")
	_testRequire.NoError(err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(provider.Close)
	d := f.deps
	d.revokeURL = provider.URL
	_, _, err = removeXSource(t.Context(), f.st, f.cfg, f.source, d, false)
	_testRequire.NoError(err)
	_, err = f.st.GetSourceByID(f.source.ID)
	_testRequire.ErrorIs(err, store.ErrSourceNotFound)
	var aliases, attempts int
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM x_post_versions").Scan(&aliases))
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM x_reply_attempts").Scan(&attempts))
	_testAssert.Zero(aliases)
	_testAssert.Zero(attempts)
}
