package mcp

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/msgsegment"
)

func TestGetMessageSegmentsOptInAndPaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	x := newExportFixture(t)
	body := "My answer π\nOn Tue, 3 Mar 2026, Someone wrote:\n> Old question\nMore of my answer\n"
	require.NoError(x.f.Store.UpsertMessageBody(x.noRaw, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	require.NoError(x.f.Store.SetReplyTo(x.f.Source.ID, "export-no-raw", "export-provider-id"))
	without := runTool[map[string]any](t, ToolGetMessage, x.h.getMessage, map[string]any{"id": float64(x.noRaw)})
	assert.NotContains(without, "segments")
	assert.NotContains(without, "segments_version")
	assert.NotContains(without, "reply_to")
	var previous []msgsegment.Segment
	for _, offset := range []int{0, 8, 20, len(body)} {
		got := runTool[getMessageResponse](t, ToolGetMessage, x.h.getMessage, map[string]any{"id": float64(x.noRaw), "include_segments": true, "offset": float64(offset), "max_chars": float64(7)})
		assert.Equal(msgsegment.Version, got.SegmentsVersion)
		require.NotNil(got.ReplyTo)
		assert.Equal(x.withRaw, got.ReplyTo.MessageID)
		require.NotNil(got.Segments)
		assert.Equal(body[got.Offset:got.Offset+got.BodyReturned], got.BodyText)
		if previous != nil {
			assert.Equal(previous, *got.Segments)
		}
		previous = *got.Segments
	}
	end := 0
	for _, s := range previous {
		assert.Equal(end, s.Start)
		require.Greater(s.End, s.Start)
		end = s.End
	}
	assert.Equal(len(body), end)
}
func TestGetMessageSegmentFormatsAndEmpty(t *testing.T) {
	x := newExportFixture(t)
	for _, tc := range []struct {
		name, typ, text, html, format string
		want                          msgsegment.Kind
	}{
		{"html", "email", "", `<p>New</p><div class="gmail_quote">Old</div>`, "html", msgsegment.Quoted},
		{"text", "email", "> Old\nNew", "<p>New</p>", "text", msgsegment.Quoted},
		{"chat", "slack", "> Literal quote syntax\n-- \nAll chat text", "", "text", msgsegment.Authored},
		{"empty", "email", "", "", "text", msgsegment.Authored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, err := x.f.Store.DB().ExecContext(t.Context(), x.f.Store.Rebind("UPDATE messages SET message_type = ? WHERE id = ?"), tc.typ, x.noRaw)
			require.NoError(err)
			require.NoError(x.f.Store.UpsertMessageBody(x.noRaw, sql.NullString{String: tc.text, Valid: true}, sql.NullString{String: tc.html, Valid: true}))
			got := runTool[getMessageResponse](t, ToolGetMessage, x.h.getMessage, map[string]any{"id": float64(x.noRaw), "include_segments": true, "body_format": tc.format, "full_body": true})
			require.NotNil(got.Segments)
			assert.Equal(tc.text, got.BodyText)
			wantHTML := ""
			if tc.format == bodyFormatHTML {
				wantHTML = tc.html
			}
			assert.Equal(wantHTML, got.BodyHTML)
			if tc.name == "empty" {
				assert.Empty(*got.Segments)
			} else {
				var kinds []msgsegment.Kind
				for _, s := range *got.Segments {
					kinds = append(kinds, s.Kind)
				}
				assert.Contains(kinds, tc.want)
			}
		})
	}
}
func TestListThreadSegmentsAndForwardTrust(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	x := newExportFixture(t)
	body := "FYI\nBegin forwarded message:\nFrom: Vendor <billing@example.com>\nDate: Monday\nSubject: Invoice\n\nPay here\n"
	require.NoError(x.f.Store.UpsertMessageBody(x.noRaw, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	ordinary := runTool[map[string]any](t, ToolListThread, x.h.listThread, map[string]any{"id": float64(x.noRaw), "offset": float64(1)})
	entries, ok := ordinary["messages"].([]any)
	require.True(ok)
	require.Len(entries, 1)
	entry, ok := entries[0].(map[string]any)
	require.True(ok)
	assert.NotContains(entry, "segments")
	assert.NotContains(entry, "new_text_bytes")
	got := runTool[threadSegmentsResponse](t, ToolListThread, x.h.listThread, map[string]any{"id": float64(x.noRaw), "offset": float64(1), "include_segments": true})
	require.Len(got.Messages, 1)
	m := got.Messages[0]
	require.NotNil(m.Segments)
	assert.Equal(len(body), *m.BodyLength)
	assert.Equal(len("FYI\n"), *m.NewTextBytes)
	require.Len(*m.Segments, 2)
	s := (*m.Segments)[1]
	assert.Equal(msgsegment.Forwarded, s.Kind)
	assert.Equal("relayed", s.Trust)
	require.NotNil(s.Claimed)
	assert.Equal("untrusted_text", s.Claimed.ContentTrust)
}
func TestSegmentsTruncation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	x := newExportFixture(t)
	body := strings.Repeat("x", msgsegment.MaxBodyBytes+5)
	require.NoError(x.f.Store.UpsertMessageBody(x.noRaw, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	got := runTool[getMessageResponse](t, ToolGetMessage, x.h.getMessage, map[string]any{"id": float64(x.noRaw), "include_segments": true})
	assert.True(got.SegmentsTruncated)
	require.NotNil(got.Segments)
	assert.Equal(len(body), (*got.Segments)[len(*got.Segments)-1].End)
}

func TestSegmentsThroughModernSDK(t *testing.T) {
	x := newExportFixture(t)
	require.NoError(t, x.f.Store.UpsertMessageBody(x.noRaw, sql.NullString{String: "New\n> Old", Valid: true}, sql.NullString{}))
	opts := ServeOptions{Engine: x.engine}
	for _, include := range []bool{false, true} {
		for _, name := range []string{ToolGetMessage, ToolListThread} {
			result := rawCallTool(t, opts, name, map[string]any{"id": x.noRaw, "include_segments": include})
			isError, _ := result["isError"].(bool)
			assert.False(t, isError, "%v", result)
			require.Contains(t, result, "structuredContent")
		}
	}
}

func TestDaemonMessageSegmentsPreserveHTMLAndMultipartBodiesAndRetryThreadRateLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	const bodyText = "Newest reply\n> Older reply"
	const bodyHTML = `<p>Newest reply</p><div class="gmail_quote">Older reply</div>`
	const htmlOnlyResponse = `{"id":42,"source_message_id":"message-42","conversation_id":7,"source_conversation_id":"thread-42","subject":"Reply","message_type":"email","body_html":"<p>Newest reply</p><div class=\"gmail_quote\">Older reply</div>"}`
	const multipartResponse = `{"id":42,"source_message_id":"message-42","conversation_id":7,"source_conversation_id":"thread-42","subject":"Reply","message_type":"email","body_text":"Newest reply\n> Older reply","body_html":"<p>Newest reply</p><div class=\"gmail_quote\">Older reply</div>"}`
	var detailRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.33.0"}`))
		case "/api/v1/cli/message":
			attempt := detailRequests.Add(1)
			if attempt == 4 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded","message":"Try again later"}`))
				return
			}
			if attempt == 1 {
				_, _ = w.Write([]byte(htmlOnlyResponse))
				return
			}
			_, _ = w.Write([]byte(multipartResponse))
		case "/api/v1/cli/message/thread":
			_, _ = w.Write([]byte(`{"account":"owner@example.test","conversation_id":7,"source_conversation_id":"thread-42","source_id":1,"source_type":"gmail","message_id":42,"source_message_id":"message-42","total":1,"offset":0,"has_more":false,"messages":[{"id":42,"source_message_id":"message-42","subject":"Reply","from":[],"to":[],"cc":[],"has_raw":false,"attachment_count":0}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	requirements.NoError(err)
	opts := ServeOptions{Engine: daemonclient.NewEngineAdapter(client)}

	messageResult := rawCallTool(t, opts, ToolGetMessage, map[string]any{"id": float64(42), "body_format": bodyFormatHTML, "include_segments": true})
	messageIsError, _ := messageResult["isError"].(bool)
	requirements.False(messageIsError, "get_message failed: %v", messageResult)
	message := decodeStructuredContent[getMessageResponse](t, messageResult)
	assertions.Equal(bodyHTML, message.BodyHTML)
	assertions.Equal(bodyFormatHTML, message.BodyFormat)
	requirements.NotNil(message.Segments)
	assertions.Contains(segmentKinds(*message.Segments), msgsegment.Quoted)

	textResult := rawCallTool(t, opts, ToolGetMessage, map[string]any{"id": float64(42), "body_format": bodyFormatText, "full_body": true, "include_segments": true})
	textMessage := decodeStructuredContent[getMessageResponse](t, textResult)
	assertions.Equal(bodyText, textMessage.BodyText)
	assertions.Empty(textMessage.BodyHTML)
	requirements.NotNil(textMessage.Segments)
	assertions.Contains(segmentKinds(*textMessage.Segments), msgsegment.Quoted)

	htmlResult := rawCallTool(t, opts, ToolGetMessage, map[string]any{"id": float64(42), "body_format": bodyFormatHTML, "full_body": true, "include_segments": true})
	htmlMessage := decodeStructuredContent[getMessageResponse](t, htmlResult)
	assertions.Equal(bodyHTML, htmlMessage.BodyHTML)
	assertions.Empty(htmlMessage.BodyText)
	requirements.NotNil(htmlMessage.Segments)
	assertions.Contains(segmentKinds(*htmlMessage.Segments), msgsegment.Quoted)

	threadResult := rawCallTool(t, opts, ToolListThread, map[string]any{"id": float64(42), "include_segments": true})
	threadIsError, _ := threadResult["isError"].(bool)
	requirements.False(threadIsError, "list_thread failed: %v", threadResult)
	thread := decodeStructuredContent[threadSegmentsResponse](t, threadResult)
	requirements.Len(thread.Messages, 1)
	assertions.Equal(bodyFormatText, thread.Messages[0].BodyFormat)
	requirements.NotNil(thread.Messages[0].BodyLength)
	assertions.Equal(len(bodyText), *thread.Messages[0].BodyLength)
	requirements.NotNil(thread.Messages[0].Segments)
	assertions.Contains(segmentKinds(*thread.Messages[0].Segments), msgsegment.Quoted)
	assertions.Equal(int32(5), detailRequests.Load(), "thread detail request should retry its 429 response")
}

func segmentKinds(segments []msgsegment.Segment) []msgsegment.Kind {
	kinds := make([]msgsegment.Kind, 0, len(segments))
	for _, segment := range segments {
		kinds = append(kinds, segment.Kind)
	}
	return kinds
}

func decodeStructuredContent[T any](t *testing.T, result map[string]any) T {
	t.Helper()
	encoded, err := json.Marshal(result["structuredContent"])
	require.NoError(t, err)
	var decoded T
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded
}
