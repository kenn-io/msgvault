package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestHTMLOnlyMIMEBodyTextRemainsCanonicalThroughIngestionAndAPI(t *testing.T) {
	t.Parallel()
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "archive@example.test")
	requirements.NoError(err)

	htmlBody := `<html><body><p>Synthetic receipt body</p></body></html>`
	raw := testemail.MakeRaw(testemail.Options{
		From:        "shop@example.test",
		To:          "archive@example.test",
		Subject:     "Synthetic receipt",
		ContentType: "text/html; charset=utf-8",
		Headers:     map[string]string{"Message-ID": "<receipt-1@example.test>"},
		Body:        htmlBody,
	})
	requirements.NoError(importer.IngestRawMessage(t.Context(), st, source.ID, "archive@example.test", "", nil,
		"source-receipt-1", "synthetic-hash", raw, time.Time{}, slog.New(slog.DiscardHandler)))

	var messageID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = ?`, "source-receipt-1").Scan(&messageID))
	server := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, slog.New(slog.DiscardHandler))
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+strconv.FormatInt(messageID, 10), nil))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())

	var detail MessageDetail
	requirements.NoError(json.NewDecoder(response.Body).Decode(&detail))
	requirements.NotNil(detail.BodyText)
	assertions.Empty(*detail.BodyText)
	assertions.Contains(detail.BodyHTML, "Synthetic receipt body")
	assertions.Contains(detail.Body, "Synthetic receipt body")
	assertions.Contains(detail.Snippet, "Synthetic receipt body")
}
