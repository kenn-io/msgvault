package mcp

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestMessageRecordsDaemonAndEmbeddedScope(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, allowed, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "allowed", "Order", "Order B-42")
	require.NoError(t, err)
	_, outside, err := testutil.CreateIndexedSourceMessage(st, "outside@example.test", "outside", "Outside", "Secret order")
	require.NoError(t, err)
	_, loginFooter, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "login-footer", "Your order receipt", "Order B-43")
	require.NoError(t, err)
	_, oneTimePayment, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "one-time-payment", "Your order receipt", "This is a one-time payment for order B-44")
	require.NoError(t, err)
	body := `<p>Order B-42</p><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42"}</script>`
	require.NoError(t, st.UpsertMessageBody(allowed, sql.NullString{String: "Order B-42", Valid: true}, sql.NullString{String: body, Valid: true}))
	loginFooterBody := `<p>Order B-43</p><p>Sign in to view your order.</p><a href="https://shop.example.test/login?return=%2Forders">Sign in</a><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-43"}</script>`
	require.NoError(t, st.UpsertMessageBody(loginFooter, sql.NullString{String: "Order B-43", Valid: true}, sql.NullString{String: loginFooterBody, Valid: true}))
	oneTimePaymentBody := `<p>This is a one-time payment for order B-44.</p><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-44"}</script>`
	require.NoError(t, st.UpsertMessageBody(oneTimePayment, sql.NullString{String: "This is a one-time payment for order B-44", Valid: true}, sql.NullString{String: oneTimePaymentBody, Valid: true}))
	bookingJSONLD := `{"@context":"https://schema.org","@type":"FlightReservation","reservationNumber":"ABC123"}`
	bookingHTML := `<html><body><p>ABC123 is your booking code.</p><script type="application/ld+json">` + bookingJSONLD + `</script></body></html>`
	rawHTMLConversation, err := st.EnsureConversation(source.ID, "raw-html-booking", "Synthetic")
	require.NoError(t, err)
	rawHTMLBooking, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: rawHTMLConversation, SourceMessageID: "raw-html-booking",
		MessageType: "email", Subject: sql.NullString{String: "Synthetic flight booking", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRaw(rawHTMLBooking, testemail.MakeRaw(testemail.Options{
		From: "shop@example.test", To: "reader@example.com", Subject: "Synthetic flight booking",
		ContentType: "text/html; charset=utf-8", Body: bookingHTML,
	})))
	multipartBody := "--booking-alt\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"ABC123 is your booking code.\r\n" +
		"--booking-alt\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		bookingHTML + "\r\n" +
		"--booking-alt--\r\n"
	rawMultipartConversation, err := st.EnsureConversation(source.ID, "raw-multipart-booking", "Synthetic")
	require.NoError(t, err)
	rawMultipartBooking, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: rawMultipartConversation, SourceMessageID: "raw-multipart-booking",
		MessageType: "email", Subject: sql.NullString{String: "Synthetic flight booking", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRaw(rawMultipartBooking, testemail.MakeRaw(testemail.Options{
		From: "shop@example.test", To: "reader@example.com", Subject: "Synthetic flight booking",
		ContentType: `multipart/alternative; boundary="booking-alt"`,
		Headers:     map[string]string{"MIME-Version": "1.0"}, Body: multipartBody,
	})))
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "records", []string{"message.read"}, []int64{source.ID}, nil, time.Time{})
	require.NoError(t, err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = agent.Close() })
	session := task5ConnectClient(t, agentServeOptions(t, agent), true)
	embedded := NewEmbeddedReader(st, "")
	t.Cleanup(embedded.Close)
	for _, mode := range []string{"daemon", "embedded"} {
		t.Run(mode, func(t *testing.T) {
			call := func(id int64) *sdkmcp.CallToolResult {
				t.Helper()
				args := map[string]any{"message_id": float64(id)}
				if mode == "daemon" {
					result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: args})
					require.NoError(t, callErr)
					return result
				}
				result, structured, callErr := embedded.Call(t.Context(), []int64{source.ID}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetMessageRecords}}, args)
				require.NoError(t, callErr)
				result.StructuredContent = structured
				return result
			}
			result := call(allowed)
			require.False(t, result.IsError, task5SDKResultText(result))
			value := task5StructuredAs[messageRecordsResponse](t, result)
			assert.Equal(t, allowed, value.MessageID)
			assert.Equal(t, "order", value.Kind)
			require.Len(t, value.Records, 1)
			assert.Equal(t, "B-42", value.Records[0].Fields["order_number"].Value)
			assert.Equal(t, "unknown", value.Records[0].Fields["order_number"].Origin)
			assert.Equal(t, "untrusted_text", value.Records[0].Fields["order_number"].Trust)
			assert.Nil(t, value.SenderAuth)
			receipt := call(loginFooter)
			require.False(t, receipt.IsError, task5SDKResultText(receipt))
			receiptValue := task5StructuredAs[messageRecordsResponse](t, receipt)
			assert.Equal(t, "order", receiptValue.Kind)
			require.Len(t, receiptValue.Records, 1)
			assert.Equal(t, "B-43", receiptValue.Records[0].Fields["order_number"].Value)
			payment := call(oneTimePayment)
			require.False(t, payment.IsError, task5SDKResultText(payment))
			paymentValue := task5StructuredAs[messageRecordsResponse](t, payment)
			assert.Equal(t, "order", paymentValue.Kind)
			require.Len(t, paymentValue.Records, 1)
			assert.Equal(t, "B-44", paymentValue.Records[0].Fields["order_number"].Value)
			for _, id := range []int64{rawHTMLBooking, rawMultipartBooking} {
				booking := call(id)
				require.False(t, booking.IsError, task5SDKResultText(booking))
				bookingValue := task5StructuredAs[messageRecordsResponse](t, booking)
				assert.Equal(t, "flight", bookingValue.Kind)
				require.Len(t, bookingValue.Records, 1)
				assert.Equal(t, "ABC123", bookingValue.Records[0].Fields["confirmation_code"].Value)
			}
			denied, missing := call(outside), call(outside+100000)
			assert.True(t, denied.IsError)
			assert.True(t, missing.IsError)
			assert.Equal(t, task5SDKResultText(missing), task5SDKResultText(denied))
		})
	}
	// The tool reads again rather than retaining extracted records.
	require.NoError(t, st.UpsertMessageBody(allowed, sql.NullString{}, sql.NullString{String: `<script type="application/ld+json">{broken`, Valid: true}))
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": allowed}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Empty(t, task5StructuredAs[messageRecordsResponse](t, result).Records)
	require.NoError(t, st.UpsertMessageBody(allowed, sql.NullString{String: "Your sign-in verification code is 654321", Valid: true}, sql.NullString{String: body, Valid: true}))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": allowed}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	value := task5StructuredAs[messageRecordsResponse](t, result)
	assert.Equal(t, "verification", value.Kind)
	assert.Empty(t, value.Records)
	assert.NotContains(t, task5SDKResultText(result), "654321")
}
