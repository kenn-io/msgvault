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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestHTMLOnlyMIMEBodyTextRemainsCanonicalThroughIngestionAndAPI(t *testing.T) {
	t.Parallel()
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "archive@example.test")
	requirements.NoError(err)

	htmlBody := `<html><body><p>Synthetic receipt body with htmlonlyneedle</p></body></html>`
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
	_, hits, err := st.SearchMessages("htmlonlyneedle", 0, 10)
	requirements.NoError(err, "search HTML-only imported body")
	assertions.Equal(int64(1), hits)

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

func TestRawMIMEFallbackPreservesBodyAlternativesThroughAPI(t *testing.T) {
	t.Parallel()
	st := testutil.NewTestStore(t)
	jsonLD := `{"@context":"https://schema.org","@type":"FlightReservation","reservationNumber":"ABC123"}`
	htmlBody := `<html><body><p>ABC123 is your booking code.</p><script type="application/ld+json">` + jsonLD + `</script></body></html>`
	multipartBody := "--booking-alt\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"ABC123 is your booking code.\r\n" +
		"--booking-alt\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		htmlBody + "\r\n" +
		"--booking-alt--\r\n"
	cases := []struct {
		name, providerID, contentType, body string
		wantText                            bool
	}{
		{
			name:        "HTML-only MIME",
			providerID:  "raw-html-booking",
			contentType: "text/html; charset=utf-8",
			body:        htmlBody,
		},
		{
			name:        "multipart alternative MIME",
			providerID:  "raw-multipart-booking",
			contentType: `multipart/alternative; boundary="booking-alt"`,
			body:        multipartBody,
			wantText:    true,
		},
	}
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	server := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			_, id, err := testutil.CreateIndexedSourceMessage(st, "archive@example.test", tc.providerID, "Synthetic flight booking", "")
			requirements.NoError(err)
			raw := testemail.MakeRaw(testemail.Options{
				From:        "shop@example.test",
				To:          "archive@example.test",
				Subject:     "Synthetic flight booking",
				ContentType: tc.contentType,
				Headers:     map[string]string{"MIME-Version": "1.0"},
				Body:        tc.body,
			})
			requirements.NoError(st.UpsertMessageRaw(id, raw))

			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+strconv.FormatInt(id, 10), nil))
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())

			var detail MessageDetail
			requirements.NoError(json.NewDecoder(response.Body).Decode(&detail))
			requirements.NotNil(detail.BodyText)
			if tc.wantText {
				assertions.Contains(*detail.BodyText, "ABC123 is your booking code")
			} else {
				assertions.Empty(*detail.BodyText)
			}
			assertions.Contains(detail.BodyHTML, `type="application/ld+json"`)
			assertions.Contains(detail.BodyHTML, `"reservationNumber":"ABC123"`)
			assertions.Contains(detail.Body, "ABC123 is your booking code")
		})
	}
}
