package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestKataEvidenceMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("evidence-message")
	_, err := f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies (message_id,body_text) VALUES (?,?)"), id, "Please send the revised budget by Friday.")
	require.NoError(err)

	svc := kataevidence.New(f.Store)
	start, end := 7, 30
	selector := kataevidence.Selector{Kind: "message", MessageID: id, StartRune: &start, EndRune: &end}
	prepared, err := svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.NoError(err)
	assert.Equal("send the revised budget", prepared[0].Excerpt)
	got, err := svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Available, got.State)
	assert.Equal(prepared[0].Excerpt, got.Evidence.Excerpt)

	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE message_bodies SET body_text=? WHERE message_id=?"), "Please send the final budget by Friday.", id)
	require.NoError(err)
	got, err = svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Changed, got.State)

	// Without a stored body or raw MIME there is nothing to cite.
	_, err = f.Store.DB().Exec(f.Store.Rebind("DELETE FROM message_bodies WHERE message_id=?"), id)
	require.NoError(err)
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnavailable)

	// A message kept only as raw MIME quotes the text the reader shows.
	rawOnly := f.CreateMessage("raw-message")
	require.NoError(f.Store.UpsertMessageRaw(rawOnly, []byte("From: a@example.com\r\nSubject: Budget\r\nContent-Type: text/plain\r\n\r\nPlease send the revised budget by Friday.\r\n")))
	selector.MessageID = rawOnly
	prepared, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.NoError(err)
	assert.Equal("send the revised budget", prepared[0].Excerpt)
	got, err = svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Available, got.State)
	assert.Equal(prepared[0].Excerpt, got.Evidence.Excerpt)
}

func TestKataEvidenceDocumentChunk(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	text := strings.Repeat("prefix ", 400) + "quasar important commitment"
	publishSearchDocument(t, f, profile, hash, text, "evidence-old")
	hits, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	require.Len(hits.Results, 1)
	hit := hits.Results[0]
	require.Greater(hit.ExcerptStartRune, 2000)

	svc := kataevidence.New(f.Store)
	start := hit.ExcerptStartRune
	end := start + len([]rune(hit.Excerpt))
	selector := kataevidence.Selector{Kind: "document_chunk", MessageID: hit.MessageID, AttachmentID: hit.AttachmentID, ExtractionID: hit.ExtractionID, ChunkKey: hit.ChunkKey, StartRune: &start, EndRune: &end}
	prepared, err := svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.NoError(err)
	assert.Equal(hit.Excerpt, prepared[0].Excerpt)
	assert.Contains(prepared[0].Excerpt, "quasar")

	publishSearchDocument(t, f, profile, hash, "new unrelated evidence", "evidence-new")
	got, err := svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Available, got.State, "a citation keeps reading its own extraction after reprocessing")
	assert.Equal(prepared[0].Excerpt, got.Evidence.Excerpt)

	missing := selector
	missing.ChunkKey = "page:000000:000000-000001"
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{missing})
	require.ErrorIs(err, kataevidence.ErrUnavailable)

	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_chunks SET checksum='' WHERE extraction_id=? AND chunk_key=?"), "evidence-old", hit.ChunkKey)
	require.NoError(err)
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnavailable, "a chunk without a checksum cannot be cited, but the request was valid")
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_chunks SET checksum=? WHERE extraction_id=? AND chunk_key=?"), prepared[0].Reference.DocumentChunk.ChunkChecksum, "evidence-old", hit.ChunkKey)
	require.NoError(err)

	tampered := prepared[0].Reference
	payload := *tampered.DocumentChunk
	payload.ChunkChecksum = strings.Repeat("f", 64)
	tampered.DocumentChunk = &payload
	got, err = svc.ResolveAround(t.Context(), tampered, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Changed, got.State)
	assert.Empty(got.Evidence.Excerpt)

	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET normalization_version=NULL WHERE id=?"), "evidence-old")
	require.NoError(err)
	got, err = svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Unprocessed, got.State)

	// A file cites only what document search finds, which leaves out messages
	// deleted at their source.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?"), hit.MessageID)
	require.NoError(err)
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnavailable)
	got, err = svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Unavailable, got.State)
}

// A lookup by row IDs names the same sources the prepared citations do.
func TestKataCitationSourceMatchesPreparedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "quasar important commitment", "citation-extraction")
	hits, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	require.Len(hits.Results, 1)
	hit := hits.Results[0]
	_, err = f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies (message_id,body_text) VALUES (?,?)"), hit.MessageID, "Please send the revised budget by Friday.")
	require.NoError(err)

	start, end := 0, 6
	prepared, err := kataevidence.New(f.Store).Prepare(t.Context(), []kataevidence.Selector{
		{Kind: "message", MessageID: hit.MessageID, StartRune: &start, EndRune: &end},
		{Kind: "document_chunk", MessageID: hit.MessageID, AttachmentID: hit.AttachmentID, ExtractionID: hit.ExtractionID, ChunkKey: hit.ChunkKey, StartRune: &start, EndRune: &end},
	})
	require.NoError(err)
	message, err := f.Store.KataCitationSource(t.Context(), hit.MessageID, 0)
	require.NoError(err)
	assert.Equal(kataevidence.SourceKeys(prepared[0].Reference), kataevidence.SourceKeys(message))
	file, err := f.Store.KataCitationSource(t.Context(), hit.MessageID, hit.AttachmentID)
	require.NoError(err)
	assert.Equal(kataevidence.SourceKeys(prepared[1].Reference), kataevidence.SourceKeys(file))

	other := f.CreateMessage("other-message")
	_, err = f.Store.KataCitationSource(t.Context(), other, hit.AttachmentID)
	require.ErrorIs(err, kataevidence.ErrUnavailable)

	// Issues filed before a message left its source still name it.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?"), hit.MessageID)
	require.NoError(err)
	file, err = f.Store.KataCitationSource(t.Context(), hit.MessageID, hit.AttachmentID)
	require.NoError(err)
	assert.Equal(kataevidence.SourceKeys(prepared[1].Reference), kataevidence.SourceKeys(file))
}
