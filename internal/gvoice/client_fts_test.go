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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportIndexesMessageBodies(t *testing.T) {
	testutil.SkipIfPostgres(t, "inspects the SQLite FTS5 envelope")
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
				if kind == "Placed" {
					from = "+12025550100"
				}
			}
			filename := "Synthetic Peer - " + fileKind + " - 2024-01-02T03_04_05Z.html"
			if kind == "Group" {
				filename = "Group Conversation - 2024-01-02T03_04_05Z.html"
			}
			htmlPath := filepath.Join(voice, "Calls", filename)
			require.NoError(os.WriteFile(htmlPath, []byte(content), 0o600))
			st := testutil.NewTestStore(t)
			require.True(st.FTS5Available())
			for range 2 {
				summary := importVoice(t, st, voice, t.TempDir())
				assert.Equal(1, summary.MessagesImported)
				assert.Zero(summary.Skipped)
			}
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
			var indexedFrom, body string
			require.NoError(st.DB().QueryRow(`SELECT from_addr,body FROM messages_fts WHERE rowid = ?`, id).Scan(&indexedFrom, &body))
			assert.Equal(from, indexedFrom)
			storedBody, err := st.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal(storedBody, body)
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, id).Scan(&count))
			assert.Equal(1, count)

			// Keep the text identity's first 50 body bytes fixed and use a fresh parser cache.
			require.NoError(os.WriteFile(htmlPath, []byte(strings.ReplaceAll(content, token, "replacementmarker")), 0o600))
			importVoice(t, st, voice, t.TempDir())
			ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"replacementmarker"}}, 10)
			require.NoError(err)
			assert.Equal([]int64{id}, ids)
			ids, _, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{token}}, 10)
			require.NoError(err)
			assert.Empty(ids)

			var logs bytes.Buffer
			client := newVoiceClient(t, voice, t.TempDir(), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
			_, err = st.DB().Exec(`DROP TABLE messages_fts`)
			require.NoError(err)
			summary, err := clientImport(t, client, st)
			require.NoError(err)
			assert.Equal(1, summary.MessagesImported)
			assert.Zero(summary.Skipped)
			assert.Contains(logs.String(), "failed to index message for search")
		})
	}
}

func TestImportReimportPreservesBackfilledRecipientSearch(t *testing.T) {
	testutil.SkipIfPostgres(t, "inspects the SQLite FTS5 envelope")

	for _, kind := range []string{"Text", "Placed"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			voice := t.TempDir()
			calls := filepath.Join(voice, "Calls")
			require.NoError(os.Mkdir(calls, 0o700))
			phones := "BEGIN:VCARD\nVERSION:3.0\nitem1.TEL:+12025550100\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"
			require.NoError(os.WriteFile(filepath.Join(voice, "Phones.vcf"), []byte(phones), 0o600))

			filename := "Synthetic Contact - " + kind + " - 2024-01-02T03_04_05Z.html"
			var fixture string
			if kind == "Text" {
				fixture = `<html><body><div class="hChatLog hfeed">` +
					`<div class="message"><abbr class="dt" title="2024-01-02T03:04:05.000+00:00"></abbr><cite class="sender vcard"><a class="tel" href="tel:+12025550101"><span class="fn">Synthetic Contact</span></a></cite><q>incomingmarker</q></div>` +
					`<div class="message"><abbr class="dt" title="2024-01-02T03:05:05.000+00:00"></abbr><cite class="sender vcard"><a class="tel" href="tel:+12025550100"><abbr class="fn">Me</abbr></a></cite><q>outgoingmarker</q></div>` +
					`</div></body></html>`
			} else {
				fixture = `<html><head><title>Placed call to Synthetic Contact</title></head><body><div class="haudio"><span class="fn">Placed call to Synthetic Contact</span><div class="contributor vcard"><a class="tel" href="tel:+12025550101"><span class="fn">Synthetic Contact</span></a></div><abbr class="published" title="2024-01-02T03:04:05.000+00:00"></abbr></div></body></html>`
			}
			htmlPath := filepath.Join(calls, filename)
			require.NoError(os.WriteFile(htmlPath, []byte(fixture), 0o600))

			st := testutil.NewTestStore(t)
			require.True(st.FTS5Available())
			summary := importVoice(t, st, voice, t.TempDir())
			wantImported := 1
			if kind == "Text" {
				wantImported = 2
			}
			assert.Equal(wantImported, summary.MessagesImported)

			var messageID, participantID int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE is_from_me = 1`).Scan(&messageID))
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM participants WHERE phone_number = ?`), "+12025550101").Scan(&participantID))
			_, err := st.DB().Exec(st.Rebind(`UPDATE participants SET email_address = ? WHERE id = ?`), "recipient@example.test", participantID)
			require.NoError(err)

			_, err = st.DB().Exec(st.Rebind(`DELETE FROM messages_fts WHERE rowid = ?`), messageID)
			require.NoError(err)
			inserted, err := st.BackfillFTS(nil)
			require.NoError(err)
			assert.Equal(int64(1), inserted)

			assertBackfilledRecipientSearch(t, st, messageID)

			// Use a fresh parser cache while keeping the unchanged source identity and body.
			summary, err = clientImport(t, newVoiceClient(t, voice, t.TempDir()), st)
			require.NoError(err)
			assert.Equal(wantImported, summary.MessagesImported)
			assertBackfilledRecipientSearch(t, st, messageID)
			var toAddr string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT to_addr FROM messages_fts WHERE rowid = ?`), messageID).Scan(&toAddr))
			assert.Equal("recipient@example.test", toAddr)

			body, err := st.GetMessageBodyText(messageID)
			require.NoError(err)
			assert.NotContains(strings.ToLower(body), "recipient")
			var snippet string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT snippet FROM messages WHERE id = ?`), messageID).Scan(&snippet))
			assert.NotContains(strings.ToLower(snippet), "recipient")
		})
	}
}

func assertBackfilledRecipientSearch(t *testing.T, st *store.Store, messageID int64) {
	t.Helper()
	assert, require := assert.New(t), require.New(t)
	var toAddr string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT to_addr FROM messages_fts WHERE rowid = ?`), messageID).Scan(&toAddr))
	assert.Equal("recipient@example.test", toAddr)
	ids, total, err := st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"recipient"}}, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	assert.Equal([]int64{messageID}, ids)
}
