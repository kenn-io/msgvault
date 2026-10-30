package threads

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsWriterOwnRootOmitsParentID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, s := writerFixture(t)
	m := writerMedia("600", "own root")
	id := applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
	metadata := writerMetadata(t, s, id)
	assert.NotContains(metadata, "parent_id", "own roots must match absent/null parent selection")
	assert.Equal("600", metadata["root_id"])
	assert.Equal(true, metadata["root_owned"])
	// Replays must also remove the older writer's empty-string field.
	metadata["parent_id"] = ""
	raw, err := json.Marshal(metadata)
	require.NoError(err)
	require.NoError(s.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: string(raw), Valid: true}))
	assert.Equal(id, applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"}))
	metadata = writerMetadata(t, s, id)
	assert.NotContains(metadata, "parent_id")
	assert.NotContains(metadata, "referenced_message_id")
}

func TestThreadsWriterIgnoresUnrelatedMalformedMetadata(t *testing.T) {
	w, s := writerFixture(t)
	bad := applyWriter(t, w, writerMedia("100", "unrelated"), ItemOrigin{Kind: "own_posts"})
	// An array is valid JSON/JSONB on both backends, but is not writer metadata.
	require.NoError(t, s.SetMessageMetadataContext(t.Context(), bad, sql.NullString{String: `[]`, Valid: true}))
	id, err := w.Apply(t.Context(), writerMedia("200", "valid new root"), ItemOrigin{Kind: "own_posts"})
	require.NoError(t, err, "an unrelated malformed row must not block a valid root")
	assert.Equal(t, "valid new root", writerBody(t, s, id))
}

func TestThreadsWriterDeferredParentsRequireBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, s := writerFixture(t)
	o := ItemOrigin{Kind: "conversation", RootID: "100"}
	child := applyWriter(t, w, writerReply("102", "100", "101"), o)
	parent := applyWriter(t, w, writerReply("101", "100", "100"), o)
	link, err := s.GetMessageReplyToMessageIDContext(t.Context(), child)
	require.NoError(err)
	assert.False(link.Valid, "Apply only repairs its current item")
	require.NoError(w.ResolveParents(t.Context()))
	link, err = s.GetMessageReplyToMessageIDContext(t.Context(), child)
	require.NoError(err)
	assert.Equal(sql.NullInt64{Int64: parent, Valid: true}, link)
}

func TestThreadsWriterDeferredCrossConversationRejected(t *testing.T) {
	w, s := writerFixture(t)
	child := applyWriter(t, w, writerReply("101", "100", "200"), ItemOrigin{Kind: "conversation", RootID: "100"})
	// A newly accessible root is valid on its own. The old pending edge is
	// rejected when the explicit boundary pass can resolve its actual target.
	applyWriter(t, w, writerMedia("200", "different conversation"), ItemOrigin{Kind: "own_posts"})
	require.ErrorContains(t, w.ResolveParents(t.Context()), "cross-conversation")
	link, err := s.GetMessageReplyToMessageIDContext(t.Context(), child)
	require.NoError(t, err)
	assert.False(t, link.Valid)
}

func TestThreadsWriterSourceAndDeferredValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, s := writerFixture(t)
	otherSource, err := s.GetOrCreateSource("threads", "threads:123:789")
	require.NoError(err)
	other := NewWriter(s, otherSource.ID, "threads:123:789")
	applyWriter(t, other, writerMedia("900", "different source"), ItemOrigin{Kind: "own_posts"})
	child := applyWriter(t, w, writerReply("901", "800", "900"), ItemOrigin{Kind: "conversation", RootID: "800"})
	require.NoError(w.ResolveParents(context.Background()))
	link, err := s.GetMessageReplyToMessageIDContext(context.Background(), child)
	require.NoError(err)
	assert.False(link.Valid, "cross-source targets must stay unresolved")
	wrong := NewWriter(s, w.sourceID, "threads:123:789")
	_, err = wrong.Apply(context.Background(), writerMedia("902", "wrong binding"), ItemOrigin{Kind: "own_posts"})
	assert.Error(err)
	_, err = w.Apply(context.Background(), writerReply("901", "999", "900"), ItemOrigin{Kind: "conversation", RootID: "999"})
	assert.Error(err, "a replay cannot move a reply to a different conversation")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = w.Apply(ctx, writerMedia("903", "cancelled"), ItemOrigin{Kind: "own_posts"})
	assert.ErrorIs(err, context.Canceled)
}

func TestThreadsWriterProvisionalThenCanonical(t *testing.T) {
	assert := assert.New(t)
	w, s := writerFixture(t)
	m := writerReply("701", "700", "700")
	m.Text = new("local immutable text")
	id := applyWriter(t, w, m, ItemOrigin{Kind: "sent_provisional", RootID: "700"})
	assert.Equal(true, writerMetadata(t, s, id)["sent_provisional"])
	canonical := writerReply("701", "700", "700")
	canonical.IsReplyOwnedByMe = new(true)
	canonical.Text = new("canonical text")
	canonical.Username = new("owner")
	canonical.Permalink = new("https://www.threads.com/@owner/post/example")
	canonical.Timestamp = writerTime.Add(-1e9)
	assert.Equal(id, applyWriter(t, w, canonical, ItemOrigin{Kind: "conversation", RootID: "700"}))
	assert.Equal("canonical text", writerBody(t, s, id))
	assert.Equal(false, writerMetadata(t, s, id)["sent_provisional"])
	assert.NotEqual(true, writerMetadata(t, s, id)["text_changed"], "placeholder replacement is not a provider edit")
	applyWriter(t, w, m, ItemOrigin{Kind: "sent_provisional", RootID: "700"})
	assert.Equal("canonical text", writerBody(t, s, id))
	msg, err := s.GetMessageContext(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(canonical.Timestamp, msg.SentAt.UTC())
}

func TestThreadsWriterParentSnapshotRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, s := writerFixture(t)
	o := ItemOrigin{Kind: "conversation", RootID: "100"}
	root := applyWriter(t, w, writerMedia("100", "root"), ItemOrigin{Kind: "own_posts"})
	parent := applyWriter(t, w, writerReply("101", "100", "100"), o)
	m := writerReply("102", "100", "100")
	id := applyWriter(t, w, m, o)
	m.RepliedTo = &MediaReference{ID: "101"}
	w.afterPersist = func() error { return errors.New("interrupted parent repair") }
	_, err := w.Apply(context.Background(), m, o)
	require.ErrorContains(err, "interrupted parent repair")
	w.afterPersist = nil
	applyWriter(t, w, m, o)
	edge, err := s.GetMessageReplyToMessageIDContext(context.Background(), id)
	require.NoError(err)
	assert.Equal(parent, edge.Int64)
	m.RepliedTo = nil
	m.Present = map[string]bool{"replied_to": true}
	applyWriter(t, w, m, o)
	edge, err = s.GetMessageReplyToMessageIDContext(context.Background(), id)
	require.NoError(err)
	assert.False(edge.Valid)
	m.RepliedTo = &MediaReference{ID: "100"}
	applyWriter(t, w, m, o)
	edge, err = s.GetMessageReplyToMessageIDContext(context.Background(), id)
	require.NoError(err)
	assert.Equal(root, edge.Int64)
}

func TestThreadsWriterExplicitEmptyAndNull(t *testing.T) {
	assert := assert.New(t)
	w, s := writerFixture(t)
	m := writerMedia("800", "archived content")
	m.Username = new("author_a")
	m.MediaURL = new("https://cdn.example.invalid/image")
	id := applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
	m.Text = new("")
	m.HideStatus = new("hidden")
	applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
	assert.Equal("archived content", writerBody(t, s, id))
	m.HideStatus = new("not_hidden")
	m.Username = new("")
	m.MediaURL = nil
	m.Present = map[string]bool{"media_url": true}
	applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
	assert.Empty(writerBody(t, s, id), "explicit empty visible text replaces earlier content")
	meta := writerMetadata(t, s, id)
	assert.Nil(meta["media_url"])
	assert.Equal("", meta["current_username"])
	assert.Equal("author_a", meta["first_username"])
	assert.NotContains(meta, "text", "root/graph metadata does not duplicate bodies")
}
