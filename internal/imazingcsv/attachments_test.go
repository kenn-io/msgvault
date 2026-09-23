package imazingcsv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentIndexResolvesSafeExactAndUniqueBasenameReferences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "nested"), 0o700))
	path := filepath.Join(root, "nested", "photo.jpg")
	require.NoError(os.WriteFile(path, []byte("photo"), 0o600))
	index, err := newAttachmentIndex(root)
	require.NoError(err)

	for _, reference := range []string{"nested/photo.jpg", `nested\photo.jpg`, "photo.jpg"} {
		got, found, resolveErr := index.resolve(reference)
		require.NoError(resolveErr)
		assert.True(found)
		assert.Equal(path, got)
	}

	got, found, err := index.resolve("missing.jpg")
	require.NoError(err)
	assert.False(found)
	assert.Empty(got)
}

func TestAttachmentIndexQualifiedReferencesRequireExactMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	for _, dir := range []string{"other", "one", "two"} {
		require.NoError(os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	otherPath := filepath.Join(root, "other", "photo.jpg")
	require.NoError(os.WriteFile(otherPath, []byte("other"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(root, "one", "same.txt"), []byte("one"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(root, "two", "same.txt"), []byte("two"), 0o600))
	index, err := newAttachmentIndex(root)
	require.NoError(err)

	// A qualified reference that misses exactly must be reported missing:
	// falling back to a unique basename match would archive unrelated
	// content from another directory. Windows-style separators qualify too.
	for _, reference := range []string{
		"nested/photo.jpg", `nested\photo.jpg`, "./photo.jpg", "nested/../photo.jpg",
	} {
		got, found, resolveErr := index.resolve(reference)
		require.NoError(resolveErr, reference)
		assert.False(found, reference)
		assert.Empty(got, reference)
	}

	// A qualified miss stays missing even when several indexed files share
	// the reference's basename, rather than surfacing the bare-name
	// ambiguity error.
	got, found, resolveErr := index.resolve("three/same.txt")
	require.NoError(resolveErr)
	assert.False(found)
	assert.Empty(got)

	// Exact qualified matches keep resolving, including Windows separators,
	// and bare filenames keep the unique-basename fallback.
	got, found, resolveErr = index.resolve(`other\photo.jpg`)
	require.NoError(resolveErr)
	assert.True(found)
	assert.Equal(otherPath, got)
	got, found, resolveErr = index.resolve("photo.jpg")
	require.NoError(resolveErr)
	assert.True(found)
	assert.Equal(otherPath, got)
}

func TestAttachmentIndexRejectsUnsafeReferences(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "one"), 0o700))
	require.NoError(os.MkdirAll(filepath.Join(root, "two"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "one", "same.txt"), []byte("one"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(root, "two", "same.txt"), []byte("two"), 0o600))
	require.NoError(os.Symlink(filepath.Join(root, "one", "same.txt"), filepath.Join(root, "link.txt")))
	require.NoError(os.Symlink(filepath.Join(root, "one"), filepath.Join(root, "linked-dir")))
	index, err := newAttachmentIndex(root)
	require.NoError(err)

	for _, reference := range []string{
		"../same.txt", "/tmp/same.txt", `C:\same.txt`, "one", "link.txt", "linked-dir/other.txt",
	} {
		_, _, resolveErr := index.resolve(reference)
		require.Error(resolveErr, reference)
	}
	resolved, found, err := index.resolve("same.txt")
	require.NoError(err)
	assert.False(t, found, "an ambiguous basename must not choose either file")
	assert.Empty(t, resolved)
}

func TestAttachmentIndexAllowsMissingAttachmentsDirectory(t *testing.T) {
	index, err := newAttachmentIndex(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	_, found, err := index.resolve("photo.jpg")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestAttachmentWriteMapsAttachmentMediaType(t *testing.T) {
	tests := []struct {
		name      string
		row       Row
		reference string
		wantMIME  string
		wantMedia string
	}{
		{
			name:      "provider image type",
			row:       Row{AttachmentType: "image/jpeg"},
			reference: "photos/pic.jpg",
			wantMIME:  "image/jpeg",
			wantMedia: "image",
		},
		{
			name:      "provider video type",
			row:       Row{AttachmentType: "video/quicktime"},
			reference: "clips/clip.mov",
			wantMIME:  "video/quicktime",
			wantMedia: "video",
		},
		{
			name:      "provider audio type",
			row:       Row{AttachmentType: "audio/mp4"},
			reference: "audio/voice.m4a",
			wantMIME:  "audio/mp4",
			wantMedia: "audio",
		},
		{
			name:      "pdf from provider type",
			row:       Row{AttachmentType: "application/pdf"},
			reference: "docs/contract.pdf",
			wantMIME:  "application/pdf",
			wantMedia: "document",
		},
		{
			name:      "pdf inferred from extension",
			row:       Row{},
			reference: "docs/contract.pdf",
			wantMIME:  "application/pdf",
			wantMedia: "document",
		},
		{
			name:      "text file",
			row:       Row{AttachmentType: "text/plain"},
			reference: "notes/note.txt",
			wantMIME:  "text/plain",
			wantMedia: "document",
		},
		{
			name:      "unknown type falls back to document",
			row:       Row{},
			reference: "payload.zzz",
			wantMIME:  "application/octet-stream",
			wantMedia: "document",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			write, err := attachmentWrite(test.row, test.reference)
			require.NoError(t, err)
			assert.Equal(t, test.wantMIME, write.MIMEType)
			assert.Equal(t, test.wantMedia, write.MediaType)
		})
	}
}
