package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Malformed remote responses cannot be produced by an honest Store success.
// Only the HTTP response is synthetic; the typed daemon client, SDK, native
// handler and confirmation challenge are the production implementations.
func personAttributeResponseSession(t *testing.T, preview *atomic.Value, writes *atomic.Int32) *sdkmcp.ClientSession {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			assert.Equal("/api/v1/people/7/attributes", r.URL.Path)
			w.Header().Set("ETag", `"person-7-r1"`)
			_, _ = io.WriteString(w, `{"person_id":7,"attributes":[]}`)
			return
		}
		assert.Equal("/api/v1/people/7/attributes/synthetic_json", r.URL.Path)
		assert.Equal(`"person-7-r1"`, r.Header.Get("If-Match"))
		if r.URL.Query().Get("dry_run") == "true" {
			body, ok := preview.Load().(string)
			if !assert.True(ok) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, body)
			return
		}
		writes.Add(1)
		_, _ = io.WriteString(w, `{"dry_run":false}`)
	}))
	t.Cleanup(server.Close)
	backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(backend.Close()) })
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, PersonRecords: backend, AllowProfileWrites: true}, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "contact-preview-test", Version: "1.0"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(session.Close()) })
	return session
}

func TestPersonAttributePreviewMustBeDryRun(t *testing.T) {
	for _, tool := range []string{"set_person_attribute", "clear_person_attribute"} {
		for _, body := range []string{"null", `{}`, `{"dry_run":false}`, `{"dry_run":"true"}`, `{"dry_run":`} {
			t.Run(tool+"/"+body, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				var preview atomic.Value
				preview.Store(body)
				var writes atomic.Int32
				session := personAttributeResponseSession(t, &preview, &writes)
				args := map[string]any{"person_id": 7, "etag": `"person-7-r1"`, "attribute_slug": "synthetic_json", "expected_value_id": 9}
				if tool == "set_person_attribute" {
					args["value"] = map[string]any{"type": "json", "json": map[string]any{"a": 1}}
				}
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
				require.NoError(err)
				assert.True(result.IsError, "invalid preview must fail closed")
				assert.False(result.NeedsInput(), "invalid preview must not request confirmation")
				assert.Zero(writes.Load(), "invalid preview must not execute a real mutation")
			})
		}
	}
}

type personAttributeNativePreviewFixture struct {
	session       *sdkmcp.ClientSession
	store         *store.Store
	capture       *personAttributePreviewCaptureTransport
	personID      int64
	etag          string
	valueID       int64
	ordinal       int64
	attributeSlug string
}

type personAttributePreviewCaptureTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	previews [][]byte
	// Vary only trial clocks after the real native dry run. Wall clocks can
	// repeat on Windows; confirmation must tolerate changing clocks regardless.
	trialClock atomic.Int64
}

func (t *personAttributePreviewCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || req.URL.Query().Get("dry_run") != "true" {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if err := resp.Body.Close(); err != nil {
		return nil, err
	}
	var preview map[string]json.RawMessage
	if err := json.Unmarshal(body, &preview); err != nil {
		return nil, err
	}
	selected := "value"
	clocks := []string{"active_from", "created_at"}
	if req.Method == http.MethodDelete {
		selected = "superseded"
		clocks = []string{"active_until", "superseded_at"}
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(preview[selected], &value); err != nil {
		return nil, err
	}
	clock, err := json.Marshal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(t.trialClock.Add(1)) * time.Second))
	if err != nil {
		return nil, err
	}
	for _, key := range clocks {
		var nativeClock string
		if err := json.Unmarshal(value[key], &nativeClock); err != nil {
			return nil, err
		}
		if _, err := time.Parse(time.RFC3339Nano, nativeClock); err != nil {
			return nil, fmt.Errorf("parse native trial clock %s: %w", key, err)
		}
		value[key] = clock
	}
	preview[selected], err = json.Marshal(value)
	if err != nil {
		return nil, err
	}
	body, err = json.Marshal(preview)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.previews = append(t.previews, append([]byte(nil), body...))
	t.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

func (t *personAttributePreviewCaptureTransport) previewBodies() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	previews := make([][]byte, len(t.previews))
	for i := range t.previews {
		previews[i] = append([]byte(nil), t.previews[i]...)
	}
	return previews
}

type personAttributePreviewEnvelope struct {
	DryRun     bool                         `json:"dry_run"`
	Value      *personAttributePreviewValue `json:"value"`
	Superseded *personAttributePreviewValue `json:"superseded"`
}

type personAttributePreviewValue struct {
	ID      int64 `json:"id"`
	Ordinal int64 `json:"ordinal"`
	Value   struct {
		Type string          `json:"type"`
		JSON json.RawMessage `json:"json"`
	} `json:"value"`
	ActiveFrom   string `json:"active_from"`
	ActiveUntil  string `json:"active_until"`
	CreatedAt    string `json:"created_at"`
	SupersededAt string `json:"superseded_at"`
}

func newPersonAttributeNativePreviewFixture(t *testing.T) *personAttributeNativePreviewFixture {
	t.Helper()
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	participant, err := st.EnsureParticipant("native-preview@example.test", "Native Preview", "example.test")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	const attributeSlug = "synthetic_json"
	_, err = st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{
		UniversalID: "synthetic-person-attribute-preview",
		ObjectType:  store.AttributeObjectPerson,
		Slug:        attributeSlug,
		Label:       "Synthetic JSON",
		ValueType:   store.AttributeValueJSON,
		FieldType:   store.AttributeFieldJSON,
		APIMutable:  true,
		IsDeletable: true,
	})
	require.NoError(err)
	saved, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: attributeSlug,
		Value:  store.AttributeValue{Type: store.AttributeValueJSON, JSON: json.RawMessage(`{"b":2,"a":1}`)},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.NotNil(saved.Value)
	require.Positive(saved.Value.ID, "the fixture must use a real current value ID")

	apiServer := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}},
		Store:  st,
		Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(apiServer.Shutdown(context.Background())) })
	daemonHTTP := httptest.NewServer(apiServer.Router())
	t.Cleanup(daemonHTTP.Close)
	httpClient := daemonHTTP.Client()
	capture := &personAttributePreviewCaptureTransport{base: httpClient.Transport}
	httpClient.Transport = capture
	backend, err := daemonclient.New(daemonclient.Config{
		URL: daemonHTTP.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: httpClient,
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })
	attributes, err := backend.GetPersonAttributesContext(t.Context(), person.ID)
	require.NoError(err)
	require.NotEmpty(attributes.ETag)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{
		Engine: &querytest.MockEngine{}, PersonRecords: backend, AllowProfileWrites: true,
	}, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "native-contact-preview-test", Version: "1.0"}, &sdkmcp.ClientOptions{
		MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(session.Close()) })
	return &personAttributeNativePreviewFixture{
		session: session, store: st, capture: capture, personID: person.ID,
		etag: attributes.ETag, valueID: saved.Value.ID, ordinal: saved.Value.Ordinal,
		attributeSlug: attributeSlug,
	}
}

func TestPersonAttributeConfirmationBindsStablePreview(t *testing.T) {
	hashPattern := regexp.MustCompile(`Preview SHA256: ([a-f0-9]{64})`)
	for _, tool := range []string{"set_person_attribute", "clear_person_attribute"} {
		t.Run(tool, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := newPersonAttributeNativePreviewFixture(t)
			var firstArgs, reorderedArgs string
			if tool == "set_person_attribute" {
				firstArgs = fmt.Sprintf(`{"person_id":%d,"etag":%q,"attribute_slug":%q,"expected_value_id":%d,"ordinal":%d,"value":{"type":"json","json":{"b":2,"a":1}}}`,
					fixture.personID, fixture.etag, fixture.attributeSlug, fixture.valueID, fixture.ordinal)
				reorderedArgs = fmt.Sprintf(`{"value":{"json":{"a":1,"b":2},"type":"json"},"ordinal":%d,"expected_value_id":%d,"attribute_slug":%q,"etag":%q,"person_id":%d}`,
					fixture.ordinal, fixture.valueID, fixture.attributeSlug, fixture.etag, fixture.personID)
			} else {
				firstArgs = fmt.Sprintf(`{"person_id":%d,"etag":%q,"attribute_slug":%q,"expected_value_id":%d,"ordinal":%d}`,
					fixture.personID, fixture.etag, fixture.attributeSlug, fixture.valueID, fixture.ordinal)
				reorderedArgs = fmt.Sprintf(`{"ordinal":%d,"expected_value_id":%d,"attribute_slug":%q,"etag":%q,"person_id":%d}`,
					fixture.ordinal, fixture.valueID, fixture.attributeSlug, fixture.etag, fixture.personID)
			}

			pending, err := fixture.session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: jsontext.Value(firstArgs)})
			requirements.NoError(err)
			requirements.True(pending.NeedsInput())
			prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
			requirements.True(ok)
			assertions.Contains(prompt.Message, fmt.Sprintf(`"ordinal":%d`, fixture.ordinal), "disclose the resolved native slot")

			nativePreviews := fixture.capture.previewBodies()
			requirements.Len(nativePreviews, 1)
			var first personAttributePreviewEnvelope
			requirements.NoError(json.Unmarshal(nativePreviews[0], &first))
			requirements.True(first.DryRun, "the preview must come from the native dry-run route")
			firstValue := first.Value
			if tool == "clear_person_attribute" {
				firstValue = first.Superseded
			}
			requirements.NotNil(firstValue)
			if tool == "clear_person_attribute" {
				assertions.Equal(fixture.valueID, firstValue.ID, "clear previews the current value guarded by the supplied CAS ID")
			} else {
				requirements.NotNil(first.Superseded)
				assertions.Equal(fixture.valueID, first.Superseded.ID, "set previews the value being replaced")
				assertions.NotEqual(fixture.valueID, firstValue.ID, "set previews a new value")
				assertions.NotContains(prompt.Message, fmt.Sprintf(`"id":%d`, firstValue.ID), "trial IDs do not bind confirmation")
			}
			assertions.Equal(fixture.ordinal, firstValue.Ordinal)
			assertions.Equal("json", firstValue.Value.Type)
			assertions.JSONEq(`{"a":1,"b":2}`, string(firstValue.Value.JSON))
			firstClocks := []string{firstValue.ActiveFrom, firstValue.CreatedAt}
			if tool == "clear_person_attribute" {
				firstClocks = []string{firstValue.ActiveUntil, firstValue.SupersededAt}
			}
			for _, clock := range firstClocks {
				requirements.NotEmpty(clock)
				assertions.NotContains(prompt.Message, clock, "native trial clocks do not bind confirmation")
			}

			again, err := fixture.session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: jsontext.Value(reorderedArgs)})
			requirements.NoError(err)
			requirements.True(again.NeedsInput())
			secondPrompt, ok := again.InputRequests["confirm"].(*sdkmcp.ElicitParams)
			requirements.True(ok)
			nativePreviews = fixture.capture.previewBodies()
			requirements.Len(nativePreviews, 2)
			var second personAttributePreviewEnvelope
			requirements.NoError(json.Unmarshal(nativePreviews[1], &second))
			requirements.True(second.DryRun, "the second preview must also come from the native dry-run route")
			secondValue := second.Value
			if tool == "clear_person_attribute" {
				secondValue = second.Superseded
			}
			requirements.NotNil(secondValue)
			if tool == "clear_person_attribute" {
				assertions.Equal(fixture.valueID, secondValue.ID, "clear rechecks the same current value")
			} else {
				requirements.NotNil(second.Superseded)
				assertions.Equal(fixture.valueID, second.Superseded.ID)
				assertions.NotEqual(fixture.valueID, secondValue.ID)
				assertions.NotContains(secondPrompt.Message, fmt.Sprintf(`"id":%d`, secondValue.ID), "trial IDs do not bind confirmation")
			}
			assertions.Equal(fixture.ordinal, secondValue.Ordinal)
			assertions.Equal("json", secondValue.Value.Type)
			assertions.JSONEq(`{"a":1,"b":2}`, string(secondValue.Value.JSON))
			secondClocks := []string{secondValue.ActiveFrom, secondValue.CreatedAt}
			if tool == "clear_person_attribute" {
				secondClocks = []string{secondValue.ActiveUntil, secondValue.SupersededAt}
			}
			for i, clock := range secondClocks {
				requirements.NotEmpty(clock)
				assertions.NotEqual(firstClocks[i], clock, "the transport varies native trial clocks deterministically")
				assertions.NotContains(secondPrompt.Message, clock, "native trial clocks do not bind confirmation")
			}
			assertions.Equal(prompt.Message, secondPrompt.Message, "equivalent raw JSON and changed native trial clocks must preserve disclosure and hash")
			firstHash := hashPattern.FindStringSubmatch(prompt.Message)
			secondHash := hashPattern.FindStringSubmatch(secondPrompt.Message)
			requirements.Len(firstHash, 2)
			requirements.Len(secondHash, 2)
			assertions.Equal(firstHash[1], secondHash[1])

			result, err := fixture.session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name: tool, Arguments: jsontext.Value(reorderedArgs), RequestState: pending.RequestState,
				InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}},
			})
			requirements.NoError(err)
			assertions.False(result.IsError, "a matching native preview confirmation permits one real write")
			nativePreviews = fixture.capture.previewBodies()
			requirements.Len(nativePreviews, 3, "confirmation rechecks the native preview before the real write")
			values, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), fixture.personID, store.PersonAttributeQuery{DefinitionSlug: fixture.attributeSlug})
			requirements.NoError(err)
			if tool == "set_person_attribute" {
				requirements.Len(values, 1)
				assertions.NotEqual(fixture.valueID, values[0].ID)
				assertions.JSONEq(`{"a":1,"b":2}`, string(values[0].Value.JSON))
			} else {
				assertions.Empty(values)
			}
		})
	}
}

func TestPersonAttributeRejectsUnsafeRawNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, personID, expectedID, ordinal, value string
	}{
		{"fractional person ID", "7.0000000000000001", "9", "3", `{"type":"json","json":{"a":1}}`},
		{"fractional value ID", "7", "9.0000000000000001", "3", `{"type":"json","json":{"a":1}}`},
		{"fractional ordinal", "7", "9", "3.0000000000000001", `{"type":"json","json":{"a":1}}`},
		{"oversized JSON number", "7", "9", "3", `{"type":"json","json":{"nested":9007199254740991.1}}`},
		{"negative oversized JSON number", "7", "9", "3", `{"type":"json","json":{"nested":-9007199254740991.1}}`},
		{"fractional integer value", "7", "9", "3", `{"type":"integer","integer":1.0000000000000001}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var preview atomic.Value
			preview.Store(`{"dry_run":true,"value":{"id":9,"person_id":7,"definition_id":2,"definition_slug":"synthetic_json","ordinal":3,"value":{"type":"json","json":{"a":1}},"active_from":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","source":"manual"}}`)
			var writes atomic.Int32
			session := personAttributeResponseSession(t, &preview, &writes)
			args := fmt.Sprintf(`{"person_id":%s,"etag":"\"person-7-r1\"","attribute_slug":"synthetic_json","expected_value_id":%s,"ordinal":%s,"value":%s}`, tc.personID, tc.expectedID, tc.ordinal, tc.value)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: jsontext.Value(args)})
			require.NoError(err)
			assert.True(result.IsError, "raw numeric values must be validated before SDK float conversion can round them")
			assert.False(result.NeedsInput())
			assert.Zero(writes.Load())
		})
	}
}
