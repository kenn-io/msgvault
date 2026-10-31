package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

func TestXSourceRemovalDropsCredentialsAfterCascadeDespiteRevokeFailure(t *testing.T) {
	f := newXCollectionFixture(t)
	stage := f.deps.manager(xStagedTokensDir(f.cfg))
	require.NoError(t, stage.Save(t.Context(), xTestRecord("staged-removal-access")))
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
	require.NoError(t, err, "failed remote revoke must not retain local credentials")
	assert.Positive(t, revokes.Load())
	_, err = f.deps.manager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = stage.Load(t.Context(), "1001")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestXRemoveRacesWithSync(t *testing.T) {
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
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	_, err = f.st.GetSourceByID(f.source.ID)
	require.NoError(t, err)
	_, err = f.deps.manager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	require.NoError(t, err)
	cancel()
	require.ErrorIs(t, <-finished, context.Canceled)
	// A durable uncertain receipt is a cascade obligation, not permission to
	// resend. Seed the real schema so deleting zero receipt rows is no proof.
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "101")
	require.NoError(t, err)
	var conversation int64
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), target.MessageID).Scan(&conversation))
	_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), "removal-draft", f.source.ID, conversation, "101", "channel", "101", "synthetic reviewed body", 1)
	require.NoError(t, err)
	_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), f.source.ID, "1001", "removal-draft", "attempt-1", 1, "101", "101", "synthetic-digest", "remote_unknown", "reservation-1")
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(provider.Close)
	d := f.deps
	d.revokeURL = provider.URL
	_, _, err = removeXSource(t.Context(), f.st, f.cfg, f.source, d, false)
	require.NoError(t, err)
	_, err = f.st.GetSourceByID(f.source.ID)
	assert.ErrorIs(t, err, store.ErrSourceNotFound)
	var aliases, attempts int
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM x_post_versions").Scan(&aliases))
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM x_reply_attempts").Scan(&attempts))
	assert.Zero(t, aliases)
	assert.Zero(t, attempts)
}
