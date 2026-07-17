package beeper

// Tests for the reinstall guard (anchors.go): probe verification, the
// archived-sample arbiter, and re-arming.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportAnchorMismatchFailsFast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotEmpty(state.Anchors)

	// Simulate a reinstall/re-index: the anchor message ID now maps to a
	// different message (timestamp changed).
	ch := f.chat("!e2e:beeper.local")
	for i := range ch.Msgs {
		if ch.Msgs[i].ID == state.Anchors[0].MessageID {
			ch.Msgs[i].Timestamp = ch.Msgs[i].Timestamp.Add(time.Hour)
		}
	}

	var before int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&before))

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.Error(err)
	assert.Contains(err.Error(), "re-assigned")
	assert.Contains(err.Error(), "re-add")

	// The failure is recorded on the sync run, and the resume state was
	// checkpointed before the anchor check so no progress is lost.
	var status string
	var cursorBefore sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT status, cursor_before FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), src.ID).Scan(&status, &cursorBefore))
	assert.Equal("failed", status)
	require.True(cursorBefore.Valid)
	preserved, err := LoadSyncState(cursorBefore.String)
	require.NoError(err)
	assert.NotEmpty(preserved.Chats, "early checkpoint must preserve the resume state")

	// --full must not bypass the check: it exists precisely so a repair run
	// against a reinstalled Beeper cannot silently duplicate the archive.
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal", Full: true})
	require.Error(err)
	assert.Contains(err.Error(), "re-assigned")

	var after int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&after))
	assert.Equal(before, after, "no rows may be written after an anchor mismatch")
}

func TestImportAnchorLostMessageReanchors(t *testing.T) {
	require := require.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotEmpty(state.Anchors)

	// The anchor message is deleted for everyone (gone from the API) but its
	// chat still exists: ordinary churn, not a reinstall — the sync must
	// continue and not demand a re-add.
	ch := f.chat("!e2e:beeper.local")
	for i := range ch.Msgs {
		if ch.Msgs[i].ID == state.Anchors[0].MessageID {
			ch.Msgs = append(ch.Msgs[:i], ch.Msgs[i+1:]...)
			break
		}
	}

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err, "a lost anchor message with a live chat must not fail the sync")

	// The run must re-arm a replacement anchor: persisting a nil anchor would
	// leave the reinstall guard disabled for every following run.
	run, err = st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	rearmed, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotEmpty(rearmed.Anchors, "anchor must be re-armed in the same run")
	require.NotEqual(state.Anchors[0].MessageID, rearmed.Anchors[0].MessageID)
}

func TestRearmAnchorFallsBackToKnownChats(t *testing.T) {
	require := require.New(t)

	// A quiet account can enumerate zero chats in a run; re-arming must fall
	// back to chats known from prior runs or the reinstall guard would stay
	// disabled until new activity happens to arrive.
	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, _, done := newTestImporter(t, f)
	defer done()

	state := NewSyncState()
	state.EnsureChat("!e2e:beeper.local").Done = true
	imp.rearmAnchors(context.Background(), nil, state)
	require.NotEmpty(state.Anchors, "re-arm must probe chats known from state when none were enumerated")
	require.Equal("!e2e:beeper.local", state.Anchors[0].ChatID)
}

func TestVerifyAnchorsChatChurnVsReinstall(t *testing.T) {
	require := require.New(t)

	// Two chats, anchors in both. Deleting one anchored chat is ordinary
	// churn (the other anchor still verifies); losing every anchored chat is
	// reinstall evidence and must block the account.
	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	base := time.Now().Add(-20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.addChat(&fakeChat{
		ID: "!second:beeper.local", AccountID: "signal", Network: "Signal", Title: "Second", Type: "single",
		Participants: []map[string]any{{"id": "@me:beeper.local", "isSelf": true}},
		Msgs: []fakeMsg{{ID: "s0", SortKey: 0, Timestamp: base, Text: "hello",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann"}},
		LastActivity: base,
	})
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.Len(state.Anchors, 2, "both chats must be anchored")

	// Churn: the user deletes one anchored chat entirely — the sync must
	// continue on the strength of the surviving anchor.
	f.mu.Lock()
	f.chats = f.chats[:1] // drop !second
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err, "deleting one anchored chat is churn, not a reinstall")

	// Reinstall: every anchor message and its chat are gone.
	f.mu.Lock()
	f.chats = nil
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.Error(err)
	require.Contains(err.Error(), "re-add")
}

func TestVerifyAnchorsZeroAnchorsFallsBackToArchivedSample(t *testing.T) {
	require := require.New(t)

	// A run can legitimately complete with zero anchors (all lost to churn,
	// re-arm failed). The guard must not lapse: the next run verifies against
	// recently archived messages instead.
	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// Fabricate the zero-anchor baseline a failed re-arm would leave behind.
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	state.Anchors = nil
	blob, err := state.Marshal()
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "beeper")
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, blob))

	// Intact installation: the archived sample verifies and the run passes
	// (and re-arms).
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	run, err = st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	rearmed, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotEmpty(rearmed.Anchors, "the passing run must re-arm")

	// Reinstalled installation (IDs re-assigned): strip anchors again and
	// shift every message's timestamp — the sample must block the run.
	syncID, err = st.StartSync(src.ID, "beeper")
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, blob))
	ch := f.chat("!e2e:beeper.local")
	for i := range ch.Msgs {
		ch.Msgs[i].Timestamp = ch.Msgs[i].Timestamp.Add(2 * time.Hour)
	}
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.Error(err)
	require.Contains(err.Error(), "re-add")
}
