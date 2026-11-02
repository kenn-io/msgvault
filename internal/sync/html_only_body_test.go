package sync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestFullSyncStoresHTMLOnlyMailWithoutSynthesizingBodyText(t *testing.T) {
	t.Parallel()
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	htmlBody := `<html><body><p>Synthetic receipt body with htmlonlyneedle</p></body></html>`
	raw := email.MakeRaw(email.Options{
		From:        "shop@example.test",
		To:          testEmail,
		Subject:     "Synthetic receipt",
		ContentType: "text/html; charset=utf-8",
		Headers:     map[string]string{"Message-ID": "<receipt-1@example.test>"},
		Body:        htmlBody,
	})
	env.Mock.AddMessage("html-only", raw, []string{"INBOX"})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	runFullSync(t, env)

	var bodyText, bodyHTML sql.NullString
	requirements.NoError(env.Store.DB().QueryRow(`
		SELECT b.body_text, b.body_html
		FROM messages m
		JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_id = ? AND m.source_message_id = ?`, source.ID, "html-only").Scan(&bodyText, &bodyHTML))
	assertions.False(bodyText.Valid)
	assertions.True(bodyHTML.Valid)
	assertions.Contains(bodyHTML.String, "Synthetic receipt body")
	_, hits, err := env.Store.SearchMessages("htmlonlyneedle", 0, 10)
	requirements.NoError(err, "search HTML-only synced body")
	assertions.Equal(int64(1), hits)
}
