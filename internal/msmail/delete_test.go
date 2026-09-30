package msmail

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// deleteFixture is a synced mailbox with a Deleted Items folder, and a
// deletion executor that uses the real Graph mail client.
type deleteFixture struct {
	f    *fakeGraph
	st   *store.Store
	c    *Client
	mgr  *deletion.Manager
	exec *deletion.Executor
	src  *store.Source
}

func newDeleteFixture(t *testing.T, ids ...string) *deleteFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.folders = append(f.folders, "deleteditems")
	for _, id := range ids {
		f.put(id, "inbox")
	}
	_, err := f.sync(t, st)
	require.NoError(t, err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(t, err)
	mgr, err := deletion.NewManager(t.TempDir())
	require.NoError(t, err)
	c := NewClient(f.srv.URL, func(context.Context) (string, error) { return "tok", nil }, 1000)
	return &deleteFixture{f: f, st: st, c: c, mgr: mgr, exec: deletion.NewExecutor(mgr, st, c), src: src}
}

// stage saves a pending manifest for ids and returns its ID.
func (d *deleteFixture) stage(t *testing.T, ids ...string) string {
	t.Helper()
	m := deletion.NewManifestForSource("test", ids, deletion.SourceReference{
		ID: d.src.ID, Type: d.src.SourceType, Identifier: d.src.Identifier,
	})
	require.NoError(t, d.mgr.SaveManifest(m))
	return m.ID
}

// label returns the name of a message's only label, deleted or not.
func (d *deleteFixture) label(t *testing.T, id string) string {
	t.Helper()
	var name string
	require.NoError(t, d.st.DB().QueryRow(d.st.Rebind(`
		SELECT l.name FROM messages m
		JOIN message_labels ml ON ml.message_id = m.id
		JOIN labels l ON l.id = ml.label_id
		WHERE m.source_message_id = ?`), id).Scan(&name))
	return name
}

// Trash moves messages to Deleted Items. A sync keeps the deleted mark there,
// and a restore clears it. A message the user deletes in Outlook is not marked.
func TestTrashKeepsMarkUntilRestore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := newDeleteFixture(t, "m1", "m2", "m3")

	err := d.exec.Execute(t.Context(), d.stage(t, "m1", "m2"), deletion.DefaultExecuteOptions())
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "deleteditems", "m2": "deleteditems", "m3": "inbox"}, d.f.folder)

	d.f.put("m2", "inbox")        // the user restores m2
	d.f.put("m3", "deleteditems") // the user deletes m3 in Outlook
	_, err = d.f.sync(t, d.st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "deleted", "m2": "Inbox", "m3": "Deleteditems"}, state(t, d.st))
	assert.Equal("Deleteditems", d.label(t, "m1"))
}

// A permanent delete removes the messages through the one-at-a-time fallback,
// and a later sync keeps them marked.
func TestPermanentDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := newDeleteFixture(t, "m1", "m2", "m3")
	var logs bytes.Buffer
	d.exec.WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	require.NoError(d.exec.ExecuteBatch(t.Context(), d.stage(t, "m1", "m2")))
	assert.Equal(map[string]string{"m3": "inbox"}, d.f.folder)
	assert.Empty(logs.String(), "individual Graph deletes are expected, not a failed batch")

	_, err := d.f.sync(t, d.st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "deleted", "m2": "deleted", "m3": "Inbox"}, state(t, d.st))
}

// A message that is already gone counts as deleted.
func TestDeleteMissingMessage(t *testing.T) {
	d := newDeleteFixture(t)
	var notFound *gmail.NotFoundError
	require.ErrorAs(t, d.c.TrashMessage(t.Context(), "nope"), &notFound)
	require.ErrorAs(t, d.c.DeleteMessage(t.Context(), "nope"), &notFound)
}

// A token without Mail.ReadWrite gets 403. The run stops at the first message
// instead of failing every one.
func TestDeleteAccessDeniedStopsRun(t *testing.T) {
	assert := assert.New(t)
	d := newDeleteFixture(t, "m1", "m2")
	d.f.denied = true

	err := d.exec.Execute(t.Context(), d.stage(t, "m1", "m2"), deletion.DefaultExecuteOptions())
	require.ErrorIs(t, err, msgraph.ErrForbidden)
	assert.Equal(map[string]string{"m1": "inbox", "m2": "inbox"}, d.f.folder)
	assert.Equal(map[string]string{"m1": "Inbox", "m2": "Inbox"}, state(t, d.st))
}
