package store_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestEmailFamilyDecisionLinkPreviewRetryUndo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	a := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
	b := f.persist(attrMail{raw: "Message-ID: <b@example.test>\r\n\r\n"})
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("a", 32), Kind: "link", MessageIDs: []int64{b, a}, ActorKind: "cli"}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Len(preview.Family.Members, 2)
	before, err := f.st.EmailFamilyContext(t.Context(), a, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(before.Members, 1)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	applied, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal("active", applied.Decision.State)
	assert.Len(applied.Family.Members, 2)
	input.MessageIDs = []int64{a, b}
	retry, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(applied.Decision.ID, retry.Decision.ID)
	conflict := input
	conflict.Kind = "handled"
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), conflict)
	require.ErrorIs(err, store.ErrEmailFamilyRequestConflict)
	_, err = f.st.UndoEmailFamilyDecisionContext(t.Context(), applied.Decision.ID, 1, "stale", store.EmailFamilyOptions{})
	require.ErrorIs(err, store.ErrEmailFamilyChanged)
	undone, err := f.st.UndoEmailFamilyDecisionContext(t.Context(), applied.Decision.ID, 1, applied.Family.Revision, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.EqualValues(2, undone.Decision.Revision)
	assert.Equal("undone", undone.Decision.State)
	assert.Len(undone.Family.Members, 2) // union remains useful for disconnected copies
	again, err := f.st.UndoEmailFamilyDecisionContext(t.Context(), applied.Decision.ID, 1, "old", store.EmailFamilyOptions{})
	require.NoError(err)
	assert.EqualValues(2, again.Decision.Revision)
	retry, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal("undone", retry.Decision.State)
	after, err := f.st.EmailFamilyContext(t.Context(), a, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(after.Members, 1)
	history, err := f.st.ListEmailFamilyDecisionsContext(t.Context(), a, store.EmailFamilyOptions{})
	require.NoError(err)
	require.Len(history, 1)
	assert.Equal("undone", history[0].State)
}

func TestEmailFamilyDecisionStaleEvidenceAndScope(t *testing.T) {
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	a := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
	b := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("b", 32), Kind: "handled", MessageIDs: []int64{a}}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	require.NoError(f.st.UpsertMessageRawWithFormat(b, []byte("Message-ID: <a@example.test>\r\nReferences: <extra@example.test>\r\n\r\n"), "mime"))
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.ErrorIs(err, store.ErrEmailFamilyChanged)
	preview, err = f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	result, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	denied := sourcescope.WithSourceIDs(t.Context(), []int64{})
	_, err = f.st.ApplyEmailFamilyDecisionContext(denied, input)
	require.ErrorIs(err, store.ErrNotFound)
	_, err = f.st.UndoEmailFamilyDecisionContext(denied, result.Decision.ID, 1, result.Family.Revision, store.EmailFamilyOptions{})
	require.ErrorIs(err, store.ErrNotFound)
}

func TestEmailFamilyDecisionIncompleteEndpointsCannotBridge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	ids := []int64{}
	for i := range 3 {
		ids = append(ids, f.persist(attrMail{raw: fmt.Sprintf("Message-ID: <m%d@example.test>\r\n\r\n", i)}))
	}
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("c", 32), Kind: "link", MessageIDs: ids}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	result, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind("DELETE FROM messages WHERE id=?"), ids[2])
	require.NoError(err)
	family, err := f.st.EmailFamilyContext(t.Context(), ids[0], store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(family.Members, 1)
	replay, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(result.Decision.ID, replay.Decision.ID)
}

func TestEmailFamilyDecisionDisjointTargetsUseWholeFamilyRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "concurrent@example.test")
	a := f.persist(attrMail{raw: "Message-ID: <same@example.test>\r\n\r\n"})
	b := f.persist(attrMail{raw: "Message-ID: <same@example.test>\r\n\r\n"})
	first := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("d", 32), Kind: "handled", MessageIDs: []int64{a}}
	second := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("e", 32), Kind: "handled", MessageIDs: []int64{b}}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), first)
	require.NoError(err)
	first.ExpectedFamilyRevision = preview.BeforeRevision
	second.ExpectedFamilyRevision = preview.BeforeRevision
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, input := range []store.EmailFamilyDecisionInput{first, second} {
		go func() { <-start; _, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input); results <- err }()
	}
	close(start)
	errs := []error{<-results, <-results}
	success, stale := 0, 0
	for _, err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrEmailFamilyChanged) {
			stale++
		} else {
			require.NoError(err)
		}
	}
	assert.Equal(1, success)
	assert.Equal(1, stale)
}

func TestEmailFamilyDecisionDedupPreservesCascadeDeficitAndRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "dedup@example.test")
	ids := []int64{}
	for i := range 4 {
		ids = append(ids, f.persist(attrMail{raw: fmt.Sprintf("Message-ID: <separate%d@example.test>\r\n\r\n", i)}))
	}
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("f", 32), Kind: "link", MessageIDs: ids}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	applied, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind("DELETE FROM messages WHERE id=?"), ids[3])
	require.NoError(err)
	_, err = f.st.MergeDuplicates(ids[0], []int64{ids[1]}, "family-decision")
	require.NoError(err)
	history, err := f.st.ListEmailFamilyDecisionsContext(t.Context(), ids[0], store.EmailFamilyOptions{})
	require.NoError(err)
	require.Len(history, 1)
	assert.Equal(3, history[0].ExpectedMemberCount)
	assert.Len(history[0].MessageIDs, 2)
	assert.Equal(applied.Decision.RequestSHA256, history[0].RequestSHA256)
	family, err := f.st.EmailFamilyContext(t.Context(), ids[0], store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(family.Members, 1)
	replay, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(applied.Decision.ID, replay.Decision.ID)
	_, err = f.st.UndoDedup("family-decision")
	require.NoError(err)
	family, err = f.st.EmailFamilyContext(t.Context(), ids[1], store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(family.Members, 1)
}

func TestEmailFamilyDecisionHandledCopiesAndSurvivingEndpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", "handled@example.test")
	f.confirm("handled@example.test")
	ids := []int64{}
	for range 2 {
		ids = append(ids, f.persist(attrMail{raw: "Message-ID: <incoming@example.test>\r\n\r\n", from: []string{"external@example.test"}, to: []string{"handled@example.test"}}))
	}
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("1", 32), Kind: "handled", MessageIDs: []int64{ids[0]}}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.NotEqual("handled", preview.Family.Handling.State)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	input.RequestKey = strings.Repeat("2", 32)
	input.MessageIDs = ids
	preview, err = f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal("handled", preview.Family.Handling.State)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind("DELETE FROM messages WHERE id=?"), ids[0])
	require.NoError(err)
	family, err := f.st.EmailFamilyContext(t.Context(), ids[1], store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal("handled", family.Handling.State)
}

func TestEmailFamilyDecisionScopeCannotUseHiddenBridge(t *testing.T) {
	require := require.New(t)
	f := newAttrFixture(t, "imap", "visible@example.test")
	g := newAttrFixtureOn(t, f.st, "imap", "excluded@example.test")
	a := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
	b := f.persist(attrMail{raw: "Message-ID: <b@example.test>\r\n\r\n"})
	c := g.persist(attrMail{raw: "Message-ID: <c@example.test>\r\n\r\n"})
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("3", 32), Kind: "link", MessageIDs: []int64{a, b, c}}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.source.ID})
	family, err := f.st.EmailFamilyContext(ctx, a, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(t, family.Members, 1)
	_, err = f.st.ApplyEmailFamilyDecisionContext(ctx, input)
	require.ErrorIs(err, store.ErrNotFound)
}

func TestEmailFamilyDecisionEdgeCapRejectsMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "edge-cap@example.test")
	id := f.persist(attrMail{raw: "Message-ID: <bounded@example.test>\r\n\r\n"})
	// Fixture construction creates a fan-out that would be expensive to produce
	// through repeated previews. The resolver and mutation run their real paths.
	tx, err := f.st.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	for i := range 2001 {
		var decisionID int64
		err := tx.QueryRowContext(t.Context(), f.st.Rebind(`INSERT INTO email_family_decisions(request_key,kind,request_sha256,expected_member_count,actor_kind) VALUES(?,'handled',?,1,'owner') RETURNING id`), fmt.Sprintf("%032x", i), strings.Repeat("0", 64)).Scan(&decisionID)
		require.NoError(err)
		_, err = tx.ExecContext(t.Context(), f.st.Rebind(`INSERT INTO email_family_decision_members(decision_id,message_id) VALUES(?,?)`), decisionID, id)
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("b", 32), Kind: "handled", MessageIDs: []int64{id}}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.True(preview.Family.Truncated)
	assert.Equal("unknown", preview.Family.Handling.State)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.ErrorIs(err, store.ErrEmailFamilyTruncated)
	history, err := f.st.ListEmailFamilyDecisionsContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(history, 20)
}

func TestEmailFamilyDecisionEdgeCapIncludesProjectedMemberships(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "projected-edge-cap@example.test")
	prepareEmailFamilyHandlingFixture(t, f)
	id := persistAttributedIncoming(t, f, "bounded@example.test", "", 1)
	tx, err := f.st.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	for i := range 2000 {
		var decisionID int64
		err := tx.QueryRowContext(t.Context(), f.st.Rebind(`INSERT INTO email_family_decisions(request_key,kind,request_sha256,expected_member_count,actor_kind) VALUES(?,'handled',?,1,'owner') RETURNING id`), fmt.Sprintf("%032x", i), strings.Repeat("0", 64)).Scan(&decisionID)
		require.NoError(err)
		_, err = tx.ExecContext(t.Context(), f.st.Rebind(`INSERT INTO email_family_decision_members(decision_id,message_id) VALUES(?,?)`), decisionID, id)
		require.NoError(err)
	}
	require.NoError(tx.Commit())

	before, err := f.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.False(before.Truncated)
	assert.Equal("handled", before.Handling.State)
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("c", 32), Kind: "handled", MessageIDs: []int64{id}}
	preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(before.Revision, preview.BeforeRevision)
	assert.True(preview.Family.Truncated)
	assert.Equal("unknown", preview.Family.Handling.State)
	assert.Contains(preview.Family.Handling.Reasons, "family_truncated")

	input.ExpectedFamilyRevision = preview.BeforeRevision
	_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	require.ErrorIs(err, store.ErrEmailFamilyTruncated)
	var count int
	require.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM email_family_decisions`).Scan(&count))
	assert.Equal(2000, count)
}

func TestEmailFamilyDecisionSubsetKeepsOnlyCompleteOriginalPopulation(t *testing.T) {
	req := require.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("subset copies SQLite files")
	}
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(path)
	req.NoError(err)
	t.Cleanup(func() { req.NoError(st.Close()) })
	req.NoError(st.InitSchemaContext(t.Context()))
	f := newAttrFixtureOn(t, st, "imap", "subset-decisions@example.test")
	ids := []int64{}
	for i := range 2 {
		ids = append(ids, f.persist(attrMail{raw: fmt.Sprintf("Message-ID: <m%d@example.test>\r\n\r\n", i)}))
	}
	input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("c", 32), Kind: "link", MessageIDs: ids}
	preview, err := st.PreviewEmailFamilyDecisionContext(t.Context(), input)
	req.NoError(err)
	input.ExpectedFamilyRevision = preview.BeforeRevision
	applied, err := st.ApplyEmailFamilyDecisionContext(t.Context(), input)
	req.NoError(err)
	for _, count := range []int{1, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			directory := t.TempDir()
			_, err := store.CopySubset(path, directory, count, false)
			require.NoError(err)
			copied, err := store.Open(filepath.Join(directory, "msgvault.db"))
			require.NoError(err)
			defer func() { require.NoError(copied.Close()) }()
			var decisions int
			require.NoError(copied.DB().QueryRow("SELECT COUNT(*) FROM email_family_decisions").Scan(&decisions))
			if count == 1 {
				assert.Zero(decisions)
			} else {
				assert.Equal(1, decisions)
				var hash string
				require.NoError(copied.DB().QueryRow("SELECT request_sha256 FROM email_family_decisions").Scan(&hash))
				assert.Equal(applied.Decision.RequestSHA256, hash)
			}
		})
	}
}

func TestEmailFamilyDecisionUnionCapAndFullyCascadedReplay(t *testing.T) {
	t.Run("union cap", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newAttrFixture(t, "imap", "union@example.test")
		ids := []int64{}
		for i := range 501 {
			key := "first"
			if i >= 250 {
				key = "second"
			}
			id := f.persist(attrMail{raw: fmt.Sprintf("Message-ID: <%s@example.test>\r\n\r\n", key)})
			if i == 0 || i == 250 {
				ids = append(ids, id)
			}
		}
		input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("d", 32), Kind: "link", MessageIDs: ids}
		preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
		require.NoError(err)
		assert.True(preview.Family.Truncated)
		assert.Len(preview.Family.Members, 500)
		input.ExpectedFamilyRevision = preview.BeforeRevision
		_, err = f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
		require.ErrorIs(err, store.ErrEmailFamilyTruncated)
	})
	t.Run("fully cascaded replay", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newAttrFixture(t, "imap", "cascade@example.test")
		id := f.persist(attrMail{raw: "Message-ID: <gone@example.test>\r\n\r\n"})
		input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("e", 32), Kind: "handled", MessageIDs: []int64{id}}
		preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
		require.NoError(err)
		input.ExpectedFamilyRevision = preview.BeforeRevision
		applied, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
		require.NoError(err)
		_, err = f.st.DB().Exec(f.st.Rebind("DELETE FROM messages WHERE id=?"), id)
		require.NoError(err)
		_, err = f.st.ApplyEmailFamilyDecisionContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), input)
		require.ErrorIs(err, store.ErrNotFound)
		replay, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
		require.NoError(err)
		assert.Equal(applied.Decision.ID, replay.Decision.ID)
		undone, err := f.st.UndoEmailFamilyDecisionContext(t.Context(), applied.Decision.ID, replay.Decision.Revision, replay.Family.Revision, store.EmailFamilyOptions{})
		require.NoError(err)
		assert.Equal("undone", undone.Decision.State)
	})
}

func TestEmailFamilyDecisionReadSurvivingUnion(t *testing.T) {
	for _, deletion := range []string{"soft", "hard"} {
		t.Run(deletion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newAttrFixture(t, "imap", "undo@example.test")
			a := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
			b := f.persist(attrMail{raw: "Message-ID: <b@example.test>\r\n\r\n"})
			input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("f", 32), Kind: "link", MessageIDs: []int64{a, b}}
			preview, err := f.st.PreviewEmailFamilyDecisionContext(t.Context(), input)
			require.NoError(err)
			input.ExpectedFamilyRevision = preview.BeforeRevision
			applied, err := f.st.ApplyEmailFamilyDecisionContext(t.Context(), input)
			require.NoError(err)
			for i, id := range []int64{b, a} {
				query := "UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"
				if deletion == "hard" {
					query = "DELETE FROM messages WHERE id=?"
				}
				_, err = f.st.DB().Exec(f.st.Rebind(query), id)
				require.NoError(err)
				current, err := f.st.GetEmailFamilyDecisionContext(t.Context(), applied.Decision.ID, store.EmailFamilyOptions{})
				require.NoError(err)
				assert.Len(current.Family.Members, 1-i)
				assert.NotEqual(applied.Family.Revision, current.Family.Revision)
				_, err = f.st.GetEmailFamilyDecisionContext(t.Context(), applied.Decision.ID, store.EmailFamilyOptions{SourceIDs: []int64{}})
				require.ErrorIs(err, store.ErrNotFound)
				if i == 1 {
					undone, err := f.st.UndoEmailFamilyDecisionContext(t.Context(), current.Decision.ID, current.Decision.Revision, current.Family.Revision, store.EmailFamilyOptions{})
					require.NoError(err)
					assert.Equal("undone", undone.Decision.State)
				}
			}
			_, err = f.st.GetEmailFamilyDecisionContext(t.Context(), applied.Decision.ID+9999, store.EmailFamilyOptions{})
			require.ErrorIs(err, store.ErrNotFound)
		})
	}
}
