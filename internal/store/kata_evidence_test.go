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
	"go.kenn.io/msgvault/internal/store"
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
	id := f.CreateMessage("historical-document")
	uid, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	ref := kataevidence.Reference{Version: 1, Kind: "document_chunk", ArchiveUID: uid, MessageID: id, SourceType: f.Source.SourceType, SourceIdentifier: f.Source.Identifier, SourceMessageID: "historical-document", AttachmentID: 7, OccurrenceKey: "occurrence", DocumentChunk: &kataevidence.DocumentReference{CanonicalBlobHash: strings.Repeat("a", 64), ExtractionID: "extraction", ManifestChecksum: strings.Repeat("b", 64), ChunkKey: "chunk", ChunkChecksum: strings.Repeat("c", 64), EndRune: 12}}
	_, err = f.Store.ReadKataEvidenceSource(t.Context(), ref)
	require.ErrorIs(t, err, kataevidence.ErrUnavailable)
}

func TestKataCitationSourceMatchesLegacyDocumentCitation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partKey any
		key     string
	}{
		{"stable", "mime:2", "dococc_fca76aec6b7772dcd390d7dc77234efaf2a0c365aad316719a75350baf8c215e"},
		{"null", nil, "dococc_99373c3722401a27c9e5a6ce6a90774c733c01f1684fcbbb8b759bc989e320f4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			overriding := ""
			if f.Store.IsPostgreSQL() {
				overriding = " OVERRIDING SYSTEM VALUE"
			}
			_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type)`+overriding+` VALUES (4101,?,?,'historical-document','email')`), f.Source.ID, f.ConvID)
			require.NoError(err)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO attachments (id,message_id,filename,storage_path,source_part_key)`+overriding+` VALUES (4201,4101,'budget.pdf','',?)`), tc.partKey)
			require.NoError(err)
			uid, err := f.Store.ArchiveUIDContext(t.Context())
			require.NoError(err)
			// A saved issue cites the file key produced before the document index was retired.
			citation := kataevidence.Reference{Version: 1, Kind: "document_chunk", ArchiveUID: uid, MessageID: 4101, SourceType: f.Source.SourceType, SourceIdentifier: f.Source.Identifier, SourceMessageID: "historical-document", AttachmentID: 4201, OccurrenceKey: tc.key}
			savedKeys := kataevidence.SourceKeys(citation)
			refs, err := f.Store.KataCitationSource(t.Context(), 4101, 4201)
			require.NoError(err)
			var lookupKeys []string
			for _, ref := range refs {
				keys := kataevidence.SourceKeys(ref)
				lookupKeys = append(lookupKeys, keys[len(keys)-1])
			}
			assert.Contains(lookupKeys, savedKeys[len(savedKeys)-1])

			other := f.CreateMessage("other-message")
			_, err = f.Store.KataCitationSource(t.Context(), other, 4201)
			require.ErrorIs(err, kataevidence.ErrUnavailable)
		})
	}
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

func TestKataDocbankBindingDeliverySelection(t *testing.T) {
	for _, name := range []string{"retained", "replaced revision", "generated", "unknown supplied input", "pending", "revoked", "different destination", "deleted at source", "content refresh", "reused attachment"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newBeeperMediaFixture(t)
			audio := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "voice1", strings.Repeat("a", 64))
			mapping := audio.mapping("destination", "revision", "processing")
			if name == "generated" {
				mapping.ProcessingProfile = "audio-transcript"
			}
			destination := mapping.DestinationKey
			if name == "pending" || name == "revoked" {
				require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), mapping))
				prepared, err := f.Store.PrepareBeeperMediaOperation(t.Context(), retainOperation(mapping))
				require.NoError(err)
				if name == "revoked" {
					applied, err := f.Store.FinishBeeperMediaOperation(t.Context(), prepared, store.BeeperMediaResult{ErrorCode: "revoked", Revoked: true})
					require.NoError(err)
					require.True(applied)
				}
			} else {
				retainAudio(t, f.Store, mapping, "occurrence")
			}
			uid, err := f.Store.ArchiveUIDContext(t.Context())
			require.NoError(err)
			ref := kataevidence.Reference{Version: kataevidence.Version, Kind: "docbank_rendition", ArchiveUID: uid,
				MessageID: audio.messageID, AttachmentID: audio.attachmentID, SourceType: "beeper", SourceIdentifier: "signal",
				SourceMessageID: "voice1", OccurrenceKey: mapping.OccurrenceRef}
			switch name {
			case "replaced revision":
				mapping.Revision = "new-revision"
				mapping.OccurrenceJSON = `{"ref":"msgvault:voice1","revision":"new-revision"}`
				retainAudio(t, f.Store, mapping, "new-occurrence")
			case "different destination":
				destination = "other-destination"
			case "deleted at source":
				_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?"), audio.messageID)
				require.NoError(err)
			case "content refresh":
				changed := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "voice1", strings.Repeat("b", 64))
				require.Equal(audio.attachmentID, changed.attachmentID)
			case "reused attachment":
				_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE attachments SET content_hash=?, source_attachment_id=?, source_part_key=? WHERE id=?"),
					strings.Repeat("c", 64), "beeper:mxc://audio/other", "beeper:mxc://audio/other", audio.attachmentID)
				require.NoError(err)
			}
			inputID := ""
			if name != "unknown supplied input" && name != "generated" {
				inputID = "input"
				_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE beeper_media_deliveries SET supplied_input_id='input' WHERE destination_key=? AND processing_key=?"), mapping.DestinationKey, mapping.ProcessingKey)
				require.NoError(err)
			}
			binding, err := f.Store.KataDocbankBinding(t.Context(), destination, audio.attachmentID)
			if name != "retained" && name != "replaced revision" && name != "generated" {
				require.ErrorIs(err, kataevidence.ErrUnavailable)
			} else {
				require.NoError(err)
				assert.Equal(kataevidence.DocbankBinding{
					Reference: ref,
					Revision:  mapping.Revision, SourceID: "source-aaaa", SourceVersionID: "version", SuppliedInputID: inputID,
					VaultUID: "vault", ContentVersionID: "content", ContentSHA256: audio.hash, Profile: mapping.ProcessingProfile,
					Display: kataevidence.Display{Filename: "voice.wav"},
				}, binding)
			}
			switch name {
			case "content refresh", "revoked":
				sources, err := f.Store.KataCitationSource(t.Context(), audio.messageID, audio.attachmentID)
				require.NoError(err)
				require.Len(sources, 2)
				assert.Equal("docbank_rendition", sources[1].Kind)
				assert.Equal(kataevidence.SourceKeys(ref), kataevidence.SourceKeys(sources[1]))
			case "reused attachment":
				sources, err := f.Store.KataCitationSource(t.Context(), audio.messageID, audio.attachmentID)
				require.NoError(err)
				require.Len(sources, 1)
				assert.Equal("document_chunk", sources[0].Kind)
			}
		})
	}
}
