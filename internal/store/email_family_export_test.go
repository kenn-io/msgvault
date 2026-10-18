package store_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestEmailFamilyExportBytesProvenanceAndRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	raw := "Message-ID: <shared@example.test>\r\n\r\noriginal bytes\r\n"
	a := f.persist(attrMail{raw: raw})
	f.persist(attrMail{raw: raw})
	family, err := f.st.EmailFamilyContext(t.Context(), a, store.EmailFamilyOptions{})
	require.NoError(err)
	var output bytes.Buffer
	_, err = f.st.ExportEmailFamilyContext(t.Context(), a, "stale", store.EmailFamilyOptions{}, &output)
	require.ErrorIs(err, store.ErrEmailFamilyChanged)
	assert.Zero(output.Len())
	manifest, err := f.st.ExportEmailFamilyContext(t.Context(), a, family.Revision, store.EmailFamilyOptions{}, &output)
	require.NoError(err)
	assert.True(manifest.Complete)
	require.Len(manifest.Members, 2)
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	require.NoError(err)
	require.Len(archive.File, 3)
	for i, file := range archive.File[:2] {
		assert.Equal(fmt.Sprintf("copies/%d.eml", i+1), file.Name)
		reader, err := file.Open()
		require.NoError(err)
		data, err := io.ReadAll(reader)
		require.NoError(err)
		require.NoError(reader.Close())
		assert.Equal(raw, string(data))
		assert.Equal(fmt.Sprintf("%x", sha256.Sum256(data)), manifest.Members[i].SHA256)
	}
	reader, err := archive.File[2].Open()
	require.NoError(err)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	var saved store.EmailFamilyExportManifest
	require.NoError(json.Unmarshal(data, &saved))
	assert.Equal(family.Revision, saved.Revision)
	assert.Equal(manifest.Members, saved.Members)
	_, err = f.st.ExportEmailFamilyContext(t.Context(), a, family.Revision, store.EmailFamilyOptions{SourceIDs: []int64{}}, io.Discard)
	require.ErrorIs(err, store.ErrNotFound)
}

func TestEmailFamilyExportPartialRawStates(t *testing.T) {
	for _, state := range []string{"missing_raw", "encrypted_raw", "undecodable"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newAttrFixture(t, "imap", "owner@example.test")
			a := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
			switch state {
			case "missing_raw":
				_, err := f.st.DB().Exec(f.st.Rebind("DELETE FROM message_raw WHERE message_id=?"), a)
				require.NoError(err)
			case "encrypted_raw":
				_, err := f.st.DB().Exec(f.st.Rebind("UPDATE message_raw SET encryption_version=1 WHERE message_id=?"), a)
				require.NoError(err)
			case "undecodable":
				_, err := f.st.DB().Exec(f.st.Rebind("UPDATE message_raw SET compression='zlib',raw_data=? WHERE message_id=?"), []byte("bad"), a)
				require.NoError(err)
			}
			family, err := f.st.EmailFamilyContext(t.Context(), a, store.EmailFamilyOptions{})
			require.NoError(err)
			manifest, err := f.st.ExportEmailFamilyContext(t.Context(), a, family.Revision, store.EmailFamilyOptions{}, io.Discard)
			require.NoError(err)
			assert.False(manifest.Complete)
			require.Len(manifest.Members, 1)
			assert.Equal(state, manifest.Members[0].State)
			assert.Empty(manifest.Members[0].Filename)
		})
	}
}

// A real archive mutation during the first ZIP entry must not change later raw
// bytes from the export snapshot. Random bytes force the ZIP buffer to flush.
func TestEmailFamilyExportUsesOneSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	noise := make([]byte, 64<<10)
	_, err := rand.Read(noise)
	require.NoError(err)
	aRaw := append([]byte("Message-ID: <shared@example.test>\r\n\r\n"), noise...)
	a := f.persist(attrMail{raw: string(aRaw)})
	bRaw := "Message-ID: <shared@example.test>\r\n\r\nsecond original"
	b := f.persist(attrMail{raw: bRaw})
	family, err := f.st.EmailFamilyContext(t.Context(), a, store.EmailFamilyOptions{})
	require.NoError(err)
	var output bytes.Buffer
	var mutationErr error
	writer := &familyMutationWriter{writer: &output, mutate: func() {
		mutationErr = f.st.UpsertMessageRawWithFormatContext(t.Context(), b, []byte("Message-ID: <changed@example.test>\r\n\r\nreplacement"), "mime")
	}}
	_, err = f.st.ExportEmailFamilyContext(t.Context(), a, family.Revision, store.EmailFamilyOptions{}, writer)
	require.NoError(err)
	require.NoError(mutationErr)
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	require.NoError(err)
	reader, err := archive.File[1].Open()
	require.NoError(err)
	raw, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	assert.Equal(bRaw, string(raw))
	after, err := f.st.EmailFamilyContext(t.Context(), a, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(after.Members, 1)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.st.ExportEmailFamilyContext(cancelled, a, after.Revision, store.EmailFamilyOptions{}, io.Discard)
	require.ErrorIs(err, context.Canceled)
}

type familyMutationWriter struct {
	writer io.Writer
	mutate func()
	once   sync.Once
}

func (w *familyMutationWriter) Write(data []byte) (int, error) {
	w.once.Do(w.mutate)
	return w.writer.Write(data)
}
