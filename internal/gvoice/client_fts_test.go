package gvoice

import (
	"bytes"
	"context"
	"fmt"
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
	for _, kind := range []string{"Text", "outgoing Text", "Group", "Received", "Placed", "Missed", "Voicemail"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			voice := t.TempDir()
			require.NoError(os.Mkdir(filepath.Join(voice, "Calls"), 0o700))
			require.NoError(os.WriteFile(filepath.Join(voice, "Phones.vcf"), []byte("BEGIN:VCARD\nVERSION:3.0\nitem1.TEL:+12025550100\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"), 0o600))
			name := strings.Repeat("padding ", 20) + "callmarker"
			from := "+12025550101"
			fileKind := kind
			var content string
			if strings.HasSuffix(kind, "Text") || kind == "Group" {
				fileKind = "Text"
				sender := `<span class="fn">Synthetic Peer</span>`
				if kind == "outgoing Text" {
					from = "+12025550100"
					sender = `<abbr class="fn">Me</abbr>`
				}
				content = fmt.Sprintf(`<html><body><div class="hChatLog hfeed"><div class="message"><abbr class="dt" title="2024-01-02T03:04:05.000+00:00"></abbr><cite class="sender vcard"><a class="tel" href="tel:%s">%s</a></cite><q>%s textmarker</q></div></div></body></html>`, from, sender, strings.Repeat("padding ", 20))
			} else {
				phrase := map[string]string{"Received": "Received call from", "Placed": "Placed call to", "Missed": "Missed call from", "Voicemail": "Voicemail from"}[kind]
				content = fmt.Sprintf(`<html><head><title>%s %s</title></head><body><div class="haudio"><span class="fn">%s %s</span><div class="contributor vcard"><a class="tel" href="tel:+12025550101"><span class="fn">%s</span></a></div><abbr class="published" title="2024-01-02T03:04:05.000+00:00"></abbr></div></body></html>`, phrase, name, phrase, name, name)
			}
			filename := "Synthetic Peer - " + fileKind + " - 2024-01-02T03_04_05Z.html"
			if kind == "Group" {
				filename = "Group Conversation - 2024-01-02T03_04_05Z.html"
			}
			htmlPath := filepath.Join(voice, "Calls", filename)
			require.NoError(os.WriteFile(htmlPath, []byte(content), 0o600))
			st := testutil.NewTestStore(t)
			if !st.FTS5Available() {
				t.Skip("FTS5 not available")
			}
			summary := importVoice(t, st, voice, t.TempDir())
			assert.Equal(1, summary.MessagesImported)
			assert.Zero(summary.Skipped)
			var id int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&id))
			token := "callmarker"
			if fileKind == "Text" {
				token = "textmarker"
			}
			ids, total, err := st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{token}}, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			assert.Equal([]int64{id}, ids)

			if kind == "Placed" {
				_, err = st.DB().Exec(st.Rebind(`UPDATE participants SET email_address = ? WHERE phone_number = ?`), "recipient@example.test", "+12025550101")
				require.NoError(err)
				_, err = st.RebuildFTS(nil)
				require.NoError(err)
			}

			// Keep the text identity's first 50 body bytes fixed and use a fresh parser cache.
			require.NoError(os.WriteFile(htmlPath, []byte(strings.ReplaceAll(content, token, "replacementmarker")), 0o600))
			importVoice(t, st, voice, t.TempDir())
			ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"replacementmarker"}}, 10)
			require.NoError(err)
			assert.Equal([]int64{id}, ids)
			ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{token}}, 10)
			require.NoError(err)
			assert.Empty(ids)
			if kind == "Placed" {
				ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"recipient"}}, 10)
				require.NoError(err)
				assert.Equal([]int64{id}, ids)
			}
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
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, id)
	require.NoError(err)
	summary, err = clientImport(t, newVoiceClient(t, voice, t.TempDir()), st)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	_, total, err := st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"voicemail"}}, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
}
