package gvoice

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportIndexesMessageBodies(t *testing.T) {
	for _, kind := range []string{"Text", "Received"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			voice := t.TempDir()
			require.NoError(os.Mkdir(filepath.Join(voice, "Calls"), 0o700))
			require.NoError(os.WriteFile(filepath.Join(voice, "Phones.vcf"), []byte("BEGIN:VCARD\nVERSION:3.0\nitem1.TEL:+12025550100\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"), 0o600))
			content, token, imported := sampleTextHTML, "Awesome", 2
			if kind == "Received" {
				data, err := os.ReadFile(filepath.Join("testdata", "voicemail", "received-with-audio.html"))
				require.NoError(err)
				content, token, imported = string(data), "callmarker", 1
			}
			content = strings.ReplaceAll(content, "Test User", "callmarker")
			content = strings.ReplaceAll(content, token, strings.Repeat("padding ", 20)+token)
			filename := "Synthetic Peer - " + kind + " - 2024-01-02T03_04_05Z.html"
			htmlPath := filepath.Join(voice, "Calls", filename)
			require.NoError(os.WriteFile(htmlPath, []byte(content), 0o600))
			st := testutil.NewTestStore(t)
			if !st.FTS5Available() {
				t.Skip("FTS5 not available")
			}
			summary := importVoice(t, st, voice, t.TempDir())
			assert.Equal(imported, summary.MessagesImported)
			assert.Zero(summary.Skipped)
			ids, total, err := st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{token}}, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			require.Len(ids, 1)
			id := ids[0]

			// Keep the text identity's first 50 body bytes fixed and use a fresh parser cache.
			require.NoError(os.WriteFile(htmlPath, []byte(strings.ReplaceAll(content, token, "replacementmarker")), 0o600))
			importVoice(t, st, voice, t.TempDir())
			ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"replacementmarker"}}, 10)
			require.NoError(err)
			assert.Equal([]int64{id}, ids)
			ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{token}}, 10)
			require.NoError(err)
			assert.Empty(ids)
		})
	}
}

func TestImportFTSFailureContinues(t *testing.T) {
	testutil.SkipIfPostgres(t, "drops the SQLite FTS5 table")
	assert, require := assert.New(t), require.New(t)
	voice := writeVoice(t, voicemailFixture{fixture: "with-audio.html", html: withAudioHTML})
	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	_, err := st.DB().Exec(`DROP TABLE messages_fts`)
	require.NoError(err)
	var logs bytes.Buffer
	client := newVoiceClient(t, voice, t.TempDir(), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	summary, err := clientImport(t, client, st)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	assert.Contains(logs.String(), "failed to index message for search")
	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&id))
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.NotEmpty(body)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.NotEmpty(raw)
}
