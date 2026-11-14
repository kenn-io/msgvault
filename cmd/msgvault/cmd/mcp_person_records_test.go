package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personRecordFixture(t *testing.T) (*daemonMCPOperations, int64, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("synthetic@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}).Router()
	var mediaReads, patches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content") {
			mediaReads.Add(1)
		}
		if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/profile") {
			patches.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	return newDaemonMCPOperations(client, caps), person.ID, &mediaReads, &patches
}

func TestPersonRecordSDKAtomicPatchAndHistory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, id, _, _ := personRecordFixture(t)
	approved := false
	var disclosure string
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosure = request.Params.Message
		action := "decline"
		if approved {
			action = "accept"
		}
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": approved}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		requirements.NoError(err)
		return result
	}
	current := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id}))
	requirements.NotEmpty(current.ETag)
	patch := map[string]any{
		"names":          map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Example Person", "envelope": map[string]any{"source": "user"}}}},
		"contact_points": map[string]any{"add": []any{map[string]any{"address_kind": "email", "original_value": "person@example.test", "envelope": map[string]any{"source": "user"}}}},
		"addresses":      map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": "Exampleville", "envelope": map[string]any{"source": "user"}}}},
		"dates":          map[string]any{"add": []any{map[string]any{"date_kind": "custom", "date_text": "Spring 2020", "envelope": map[string]any{"source": "user"}}}},
		"categories":     map[string]any{"add": []any{map[string]any{"original_value": "Friends", "envelope": map[string]any{"source": "user"}}}},
		"media":          map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("synthetic")), "envelope": map[string]any{"source": "user"}}}},
	}
	args := map[string]any{"person_id": id, "etag": current.ETag, "patch": patch}
	assertions.True(call("patch_person_record", args).IsError)
	unchanged := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id}))
	assertions.Equal(current.ETag, unchanged.ETag)
	approved = true
	updated := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", args))
	assertions.NotEqual(current.ETag, updated.ETag)
	assertions.Contains(disclosure, "person@example.test")
	sum := sha256.Sum256([]byte("synthetic"))
	assertions.Contains(disclosure, `"data_size":9`)
	assertions.Contains(disclosure, fmt.Sprintf(`"data_sha256":"%x"`, sum))
	assertions.NotContains(disclosure, base64.StdEncoding.EncodeToString([]byte("synthetic")))
	assertions.Len(updated.Profile.Names, 1)
	assertions.Len(updated.Profile.ContactPoints, 1)
	assertions.Len(updated.Profile.Addresses, 1)
	assertions.Len(updated.Profile.Dates, 1)
	assertions.Len(updated.Profile.Categories, 1)
	requirements.Len(updated.Profile.Media, 1)
	mediaID := updated.Profile.Media[0].Envelope.ID
	media := loopToolOutput[mcpserver.PersonMediaContent](t, call("get_person_record_media", map[string]any{"person_id": id, "media_id": mediaID}))
	assertions.Equal("image/png", media.MediaType)
	assertions.Equal(base64.StdEncoding.EncodeToString([]byte("synthetic")), media.DataBase64)
	assertions.True(media.Complete)
	history := loopToolOutput[mcpserver.PersonHistoryRecord](t, call("get_person_record_history", map[string]any{"person_id": id}))
	assertions.Len(history.History.Categories, 1)
	assertions.Equal("person_revision_conflict", loopToolFailure(t, call("patch_person_record", args))["error"])
	closeArgs := map[string]any{"person_id": id, "etag": updated.ETag, "patch": map[string]any{"categories": map[string]any{"supersede": []int64{updated.Profile.Categories[0].Envelope.ID}}}}
	closed := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", closeArgs))
	assertions.Empty(closed.Profile.Categories)
}

func TestPersonRecordCapabilitiesAndRefusals(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, id, _, _ := personRecordFixture(t)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, tc := range []struct {
		name, operation string
		removeProperty  bool
	}{
		{"get_person_record_history", "getPersonProfileHistory", false}, {"patch_person_record", "patchPersonStructuredProfile", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subAssertions := assert.New(t)
			copyCaps := *capabilities
			copyCaps.Routes = slices.Clone(capabilities.Routes)
			for i := range copyCaps.Routes {
				if copyCaps.Routes[i].OperationID == tc.operation {
					if tc.removeProperty {
						copyCaps.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(copyCaps.Routes[i].RequestProperties), func(s string) bool { return s == "media" })
					} else {
						copyCaps.Routes[i].Method = http.MethodDelete
					}
				}
			}
			got := newDaemonMCPOperations(backend.client, &copyCaps).capabilities()
			subAssertions.NotContains(got, tc.name)
			if tc.removeProperty {
				subAssertions.Contains(got, "get_person_record")
				subAssertions.Contains(got, "get_person_record_history")
				subAssertions.Contains(got, "get_person_record_media")
			}
		})
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		code string
	}{
		{"get_person_record", map[string]any{"person_id": 0}, "invalid_arguments"},
		{"get_person_record_media", map[string]any{"person_id": id, "media_id": 1}, "profile_media_not_found"},
		{"patch_person_record", map[string]any{"person_id": id, "etag": "x"}, "invalid_arguments"},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		requirements.NoError(err)
		assertLoopRefusal(t, result, tc.code, false)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	record := mcpserver.StructuredPersonRecord{ETag: result.ETag}
	disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": record.ETag, "patch": map[string]any{"media": map[string]any{}}})
	requirements.NoError(err)
	assertions.NotEmpty(disclosure)
	invalid := map[string]any{"person_id": id, "etag": record.ETag, "patch": map[string]any{
		"names":      map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Valid Name", "envelope": map[string]any{"source": "user"}}}},
		"categories": map[string]any{"add": []any{map[string]any{"original_value": "Friends", "envelope": map[string]any{"source": "invalid"}}}},
	}}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", invalid)
	requirements.NoError(err)
	assertLoopRefusal(t, result, "invalid_profile_value", false)
	after, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	unchanged := mcpserver.StructuredPersonRecord{ETag: after.ETag}
	assertions.Equal(record.ETag, unchanged.ETag)
}

func TestPersonRecordLargeInlineMedia(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, reads, _ := personRecordFixture(t)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, nil)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	data := []byte(strings.Repeat("x", 3<<20))
	args := map[string]any{"person_id": id, "etag": result.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(data), "envelope": map[string]any{"source": "user"}}}}}}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	requirements.Len(updated.Profile.Media, 1)
	mediaID := updated.Profile.Media[0].Envelope.ID
	call := func(args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_record_media", Arguments: args})
		require.NoError(t, err)
		return result
	}
	invalid := call(map[string]any{"person_id": id, "media_id": mediaID, "offset": 1})
	assertions.True(invalid.IsError)
	assertions.Zero(reads.Load())
	media := loopToolOutput[mcpserver.PersonMediaContent](t, call(map[string]any{"person_id": id, "media_id": mediaID}))
	var assembled bytes.Buffer
	for !media.Complete {
		decoded, err := base64.StdEncoding.DecodeString(media.DataBase64)
		requirements.NoError(err)
		_, err = assembled.Write(decoded)
		requirements.NoError(err)
		media = loopToolOutput[mcpserver.PersonMediaContent](t, call(map[string]any{"person_id": id, "media_id": mediaID, "offset": media.Offset + media.Length, "sha256": media.SHA256}))
	}
	decoded, err := base64.StdEncoding.DecodeString(media.DataBase64)
	requirements.NoError(err)
	_, err = assembled.Write(decoded)
	requirements.NoError(err)
	assertions.Equal(data, assembled.Bytes())
	assertions.Equal("application/octet-stream", media.MediaType)
	assertions.Equal(int64(1), reads.Load())
	assertions.True(call(map[string]any{"person_id": id, "media_id": mediaID, "offset": 1, "sha256": strings.Repeat("0", 64)}).IsError)
	assertions.Equal(int64(1), reads.Load())
}

func TestPersonRecordURIOnlyAndRevisionRefusals(t *testing.T) {
	requirements := require.New(t)
	backend, id, _, _ := personRecordFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	record := mcpserver.StructuredPersonRecord{ETag: result.ETag}
	patch := map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "uri": "https://example.test/image", "envelope": map[string]any{"source": "user"}}}}}
	args := map[string]any{"person_id": id, "etag": record.ETag, "patch": patch}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	requirements.Len(updated.Profile.Media, 1)
	mediaID := updated.Profile.Media[0].Envelope.ID
	result, err = backend.ExecuteOperation(t.Context(), "get_person_record_media", map[string]any{"person_id": id, "media_id": mediaID})
	requirements.NoError(err)
	assertLoopRefusal(t, result, "profile_media_content_unavailable", false)
	args["etag"] = "*"
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	assertLoopRefusal(t, result, "invalid_if_match", false)
}

func TestPersonRecordLargeProfileRecoveryAndDisclosure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, _, patches := personRecordFixture(t)
	var disclosure string
	approvals := 0
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		disclosure = request.Params.Message
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	initial := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id}))
	etag := initial.ETag
	for batch := 0; batch < 2; batch++ {
		adds := make([]any, 200)
		for i := range adds {
			adds[i] = map[string]any{"address_kind": "postal", "free_text": fmt.Sprintf("address-%d-%d-%s", batch, i, strings.Repeat("x", 3000)), "envelope": map[string]any{"source": "user"}}
		}
		result := call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"addresses": map[string]any{"add": adds}}})
		record := loopToolOutput[mcpserver.StructuredPersonRecord](t, result)
		etag = record.ETag
		if batch == 1 {
			assertions.Nil(record.Profile)
			assertions.NotContains(result.StructuredContent, "operation_may_have_completed")
		}
	}
	var assembled bytes.Buffer
	args := map[string]any{"person_id": id}
	var first mcpserver.StructuredPersonRecord
	for {
		chunk := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", args))
		if first.ETag == "" {
			first = chunk
		}
		assertions.Equal(etag, chunk.ETag)
		requirements.NotNil(chunk.DataBase64)
		decoded, err := base64.StdEncoding.DecodeString(*chunk.DataBase64)
		requirements.NoError(err)
		_, err = assembled.Write(decoded)
		requirements.NoError(err)
		requirements.NotNil(chunk.Complete)
		if *chunk.Complete {
			break
		}
		args = map[string]any{"person_id": id, "offset": *chunk.Offset + *chunk.Length, "sha256": *chunk.SHA256}
	}
	raw, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	assertions.Equal(etag, raw.ETag)
	assertions.JSONEq(string(raw.Data), assembled.String())
	var profile generated.StructuredPersonProfile
	requirements.NoError(json.Unmarshal(assembled.Bytes(), &profile))
	requirements.Len(profile.Addresses, 400)
	var historyBytes bytes.Buffer
	historyArgs := map[string]any{"person_id": id}
	for {
		history := loopToolOutput[mcpserver.PersonHistoryRecord](t, call("get_person_record_history", historyArgs))
		requirements.NotNil(history.DataBase64)
		part, err := base64.StdEncoding.DecodeString(*history.DataBase64)
		requirements.NoError(err)
		_, err = historyBytes.Write(part)
		requirements.NoError(err)
		if *history.Complete {
			break
		}
		historyArgs = map[string]any{"person_id": id, "offset": *history.Offset + *history.Length, "sha256": *history.SHA256}
	}
	rawHistory, err := backend.ExecuteOperation(t.Context(), "get_person_record_history", map[string]any{"person_id": id})
	requirements.NoError(err)
	assertions.JSONEq(string(rawHistory.Data), historyBytes.String())
	ids := make([]int64, 200)
	for i := range ids {
		ids[i] = profile.Addresses[i].Envelope.ID
	}
	before := approvals
	missing := call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"addresses": map[string]any{"supersede": []int64{99999999}}}})
	assertions.Equal("profile_value_not_found", loopToolFailure(t, missing)["error"])
	assertions.Equal(before, approvals)
	refusal := call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"addresses": map[string]any{"supersede": ids}}})
	assertions.Equal("operation_response_limit_exceeded", loopToolFailure(t, refusal)["error"])
	assertions.Equal(before, approvals)
	assertions.Equal(int64(2), patches.Load())
	superseded := profile.Addresses[0]
	patch := map[string]any{"addresses": map[string]any{"supersede": []int64{superseded.Envelope.ID}}}
	updated := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": patch}))
	assertions.NotEqual(etag, updated.ETag)
	assertions.Contains(disclosure, *superseded.FreeText)
	assertions.NotContains(disclosure, *profile.Addresses[1].FreeText)
	stale := call("get_person_record", map[string]any{"person_id": id, "offset": 0, "sha256": *first.SHA256})
	assertions.Equal("invalid_arguments", loopToolFailure(t, stale)["error"])
	assertions.Contains(loopToolFailure(t, stale)["message"], "restart at offset 0")
	fresh := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id, "offset": 0}))
	assertions.Equal(updated.ETag, fresh.ETag)
	assertions.NotEqual(first.ETag, fresh.ETag)
	old := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id, "offset": *first.Length, "sha256": *first.SHA256}))
	assertions.Equal(etag, old.ETag)
	oldBytes, err := base64.StdEncoding.DecodeString(*old.DataBase64)
	requirements.NoError(err)
	assertions.Equal(assembled.Bytes()[*old.Offset:*old.Offset+*old.Length], oldBytes)
	newChunk := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id, "offset": *fresh.Length, "sha256": *fresh.SHA256}))
	assertions.Equal(updated.ETag, newChunk.ETag)
	assertions.Equal(int64(3), patches.Load())
}

func TestPersonRecordSupersedeLargeValue(t *testing.T) {
	backend, id, _, patches := personRecordFixture(t)
	initial, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	require.NoError(t, err)
	value := strings.Repeat("x", (1<<20)+1)
	added, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": initial.ETag, "patch": map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": value, "envelope": map[string]any{"source": "user"}}}}}})
	require.NoError(t, err)
	require.False(t, added.IsError)
	current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	require.NoError(t, err)
	var profile generated.StructuredPersonProfile
	require.NoError(t, json.Unmarshal(current.Data, &profile))
	require.Len(t, profile.Addresses, 1)
	var disclosure string
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosure = request.Params.Message
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_record", Arguments: map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"addresses": map[string]any{"supersede": []int64{profile.Addresses[0].Envelope.ID}}}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	encoded, err := json.Marshal(profile.Addresses[0], json.Deterministic(true))
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	assert.Contains(t, disclosure, fmt.Sprintf(`"sha256":"%x"`, sum))
	assert.Contains(t, disclosure, `"size"`)
	assert.NotContains(t, disclosure, value)
	assert.Equal(t, int64(2), patches.Load())
}

func TestPersonRecordOversizeRefusedBeforeApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, _, patches := personRecordFixture(t)
	approvals := 0
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	adds := make([]any, 201)
	for i := range adds {
		adds[i] = map[string]any{"name_kind": "formatted", "formatted": "Example", "envelope": map[string]any{"source": "user"}}
	}
	for _, tc := range []struct {
		patch map[string]any
		code  string
	}{
		{map[string]any{"names": map[string]any{"add": adds}}, "profile_patch_too_large"},
		{map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": strings.Repeat("x", 12<<20), "envelope": map[string]any{"source": "user"}}}}}, "profile_patch_too_large"},
		{map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": base64.StdEncoding.EncodeToString(make([]byte, (8<<20)+1)), "envelope": map[string]any{"source": "user"}}}}}, "invalid_profile_value"},
		{map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": base64.StdEncoding.EncodeToString(make([]byte, 9<<20)), "envelope": map[string]any{"source": "user"}}}}}, "profile_patch_too_large"},
		{map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": base64.StdEncoding.EncodeToString(make([]byte, 8<<20)), "envelope": map[string]any{"source": "user"}}}}, "names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": strings.Repeat("<", 300000), "envelope": map[string]any{"source": "user"}}}}}, "profile_patch_too_large"},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_record", Arguments: map[string]any{"person_id": id, "etag": "x", "patch": tc.patch}})
		requirements.NoError(err)
		assertions.Equal(tc.code, loopToolFailure(t, result)["error"])
	}
	assertions.Zero(approvals)
	assertions.Zero(patches.Load())
}

func TestPersonRecordLargePatchResponseOmitsProfile(t *testing.T) {
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		w.Header().Set("ETag", `"person-1-r2"`)
		_, err := w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
		assert.NoError(t, err)
	})
	backend := &daemonMCPOperations{client: client}
	result, handled, err := backend.executePersonRecord(t.Context(), "patch_person_record", map[string]any{"person_id": 1, "etag": `"person-1-r1"`, "patch": map[string]any{"names": map[string]any{}}})
	require.NoError(t, err)
	require.True(t, handled)
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	assert.Equal(t, `"person-1-r2"`, record.ETag)
	assert.Nil(t, record.Profile)
}
