package api

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaReferenceMapsDocumentsAndPhotos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	doc := &tg.MessageMediaDocument{Document: &tg.Document{ID: 101, AccessHash: 202, FileReference: []byte("reference"), DCID: 3, Size: 42, MimeType: "audio/ogg", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "voice.ogg"}, &tg.DocumentAttributeAudio{Voice: true}}}}
	ref, err := MediaReferenceFromMessage(doc)
	require.NoError(err)
	require.NotNil(ref)
	assert.Equal("voice_message", ref.Kind)
	assert.Equal("voice.ogg", ref.Filename)
	assert.Equal(int64(42), ref.Size)
	assert.Equal(3, ref.DCID)
	assert.Equal(&tg.InputDocumentFileLocation{ID: 101, AccessHash: 202, FileReference: []byte("reference")}, ref.Location)

	photo := &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 303, AccessHash: 404, FileReference: []byte("photo-ref"), DCID: 4, Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "s", W: 10, H: 10, Size: 20}, &tg.PhotoSize{Type: "x", W: 100, H: 100, Size: 200}}}}
	ref, err = MediaReferenceFromMessage(photo)
	require.NoError(err)
	require.NotNil(ref)
	assert.Equal("photo", ref.Kind)
	assert.Equal(int64(200), ref.Size)
	assert.Equal(&tg.InputPhotoFileLocation{ID: 303, AccessHash: 404, FileReference: []byte("photo-ref"), ThumbSize: "x"}, ref.Location)
}

func TestMediaReferenceRejectsIncompleteFiles(t *testing.T) {
	for _, media := range []tg.MessageMediaClass{
		&tg.MessageMediaDocument{Document: &tg.Document{ID: 1, DCID: 1, Size: -1}},
		&tg.MessageMediaDocument{Document: &tg.DocumentEmpty{ID: 1}},
		&tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1, DCID: 1, Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "x", W: 1, H: 1, Size: 0}}}},
	} {
		_, err := MediaReferenceFromMessage(media)
		assert.Error(t, err)
	}
}

func TestMediaReferenceRoundTripsStoredLocation(t *testing.T) {
	require := require.New(t)

	ref := &MediaReference{DCID: 3, Size: 42, Kind: "file", Filename: "synthetic.pdf", MIMEType: "application/pdf", Location: &tg.InputDocumentFileLocation{ID: 101, AccessHash: 202, FileReference: []byte("reference")}}
	stored, err := ref.liveMedia()
	require.NoError(err)
	loaded, err := mediaReferenceFromStored(stored)
	require.NoError(err)
	assert.Equal(t, ref, loaded)
	stored.SourceID = "document:102"
	_, err = mediaReferenceFromStored(stored)
	require.Error(err)
}
