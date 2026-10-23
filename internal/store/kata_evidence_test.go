package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestKataDocbankSourceEditBeforeReconciliation(t *testing.T) {
	for _, transcript := range []string{"edited transcript", ""} {
		t.Run(transcript, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newBeeperMediaFixture(t)
			audio := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "voice1", strings.Repeat("a", 64))
			writeRaw := func(text string) {
				raw := []byte(fmt.Sprintf(`{"id":"voice1","attachments":[{"id":"mxc://audio/voice1","isVoiceNote":true,"transcription":{"transcription":%q}}]}`, text))
				require.NoError(f.Store.UpsertMessageRawWithFormat(audio.messageID, raw, "beeper_json"))
			}
			writeRaw("original transcript")
			revision := func(ctx context.Context, id int64) (string, error) { return beeper.MediaRevision(ctx, f.Store, id) }
			original, err := revision(t.Context(), audio.attachmentID)
			require.NoError(err)
			mapping := audio.mapping("destination", original, "processing")
			retainAudio(t, f.Store, mapping, "occurrence")
			_, err = f.Store.DB().Exec("UPDATE beeper_media_deliveries SET supplied_input_id='input'")
			require.NoError(err)
			binding, err := f.Store.KataDocbankBinding(t.Context(), "destination", audio.attachmentID)
			require.NoError(err)
			writeRaw(transcript)
			reader := kataevidence.NewDocbankReader(f.Store, nil, func(context.Context) (string, error) { return "destination", nil }, revision)
			result, err := reader.LoadKataEvidenceSource(t.Context(), kataevidence.Selector{Kind: "docbank_rendition", MessageID: audio.messageID, AttachmentID: audio.attachmentID, MaxChars: new(10)})
			require.ErrorIs(err, kataevidence.ErrChanged)
			assert.Empty(result.Text)
			ref := binding.Reference
			ref.DocbankRendition = &kataevidence.DocbankReference{VaultUID: binding.VaultUID, ContentVersionID: binding.ContentVersionID, ContentSHA256: binding.ContentSHA256, EndRune: 10}
			result, err = reader.ReadKataEvidenceSource(t.Context(), ref)
			require.ErrorIs(err, kataevidence.ErrChanged)
			assert.Empty(result.Text)
		})
	}
}

func TestHistoricalKataDocumentCitationUnavailable(t *testing.T) {
	f := storetest.New(t)
	ref := kataevidence.Reference{Version: 1, Kind: "document_chunk", ArchiveUID: "synthetic-archive", MessageID: 42, SourceType: "gmail", SourceIdentifier: "archive@example.test", SourceMessageID: "message", AttachmentID: 7, OccurrenceKey: "occurrence", DocumentChunk: &kataevidence.DocumentReference{CanonicalBlobHash: strings.Repeat("a", 64), ExtractionID: "extraction", ManifestChecksum: strings.Repeat("b", 64), ChunkKey: "chunk", ChunkChecksum: strings.Repeat("c", 64), EndRune: 12}}
	_, err := f.Store.ReadKataEvidenceSource(t.Context(), ref)
	require.ErrorIs(t, err, kataevidence.ErrUnavailable)
}

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
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET subject=? WHERE id=?"), "Budget", id)
	require.NoError(err)
	got, err = svc.ResolveAround(t.Context(), prepared[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Changed, got.State)
	assert.Equal("Budget", got.Evidence.Display.ContainingTitle, "a changed message is still named")
	assert.Empty(got.Evidence.Excerpt)

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
