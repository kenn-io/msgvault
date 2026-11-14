package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	stdjson "encoding/json"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personRecordFixture(t *testing.T) (*daemonMCPOperations, int64, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	return personRecordFixtureWithHook(t, nil)
}

func personRecordFixtureWithHook(t *testing.T, hook func(*http.Request)) (*daemonMCPOperations, int64, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("synthetic@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}).Router()
	var mediaReads, patches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(r)
		}
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

func TestPersonRecordDisclosure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	var historyReads atomic.Int64
	var armed atomic.Bool
	var backend *daemonMCPOperations
	var id int64
	var etag string
	result := make(chan error, 1)
	backend, id, _, _ = personRecordFixtureWithHook(t, func(r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/profile/history") {
			return
		}
		historyReads.Add(1)
		if armed.CompareAndSwap(true, false) {
			updated, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Updated", "envelope": map[string]any{"source": "user"}}}}}})
			if err == nil && updated.IsError {
				err = fmt.Errorf("concurrent patch refused: %v", updated.Output)
			}
			result <- err
		}
	})
	current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	added, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"addresses": map[string]any{"add": []any{
		map[string]any{"address_kind": "postal", "free_text": "Current address", "envelope": map[string]any{"source": "user"}},
		map[string]any{"address_kind": "postal", "free_text": "Old address", "envelope": map[string]any{"source": "user", "active_until": "2020-01-01T00:00:00Z"}},
		map[string]any{"address_kind": "postal", "free_text": "Other old address", "envelope": map[string]any{"source": "user", "active_until": "2020-01-01T00:00:00Z"}},
	}}}})
	requirements.NoError(err)
	profile := operationOutput[mcpserver.StructuredPersonRecord](t, added)
	requirements.Len(profile.Profile.Addresses, 1)
	currentPatch := map[string]any{"addresses": map[string]any{"supersede": []int64{profile.Profile.Addresses[0].Envelope.ID}}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": added.ETag, "patch": currentPatch})
	requirements.NoError(err)
	assertions.Contains(disclosure, "Current address")
	assertions.Zero(historyReads.Load())
	media := base64.StdEncoding.EncodeToString([]byte("synthetic"))
	disclosure, err = backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": added.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": media, "envelope": map[string]any{"source": "user"}}}}}})
	requirements.NoError(err)
	sum := sha256.Sum256([]byte("synthetic"))
	assertions.Contains(disclosure, `"data_size":9`)
	assertions.Contains(disclosure, fmt.Sprintf(`"data_sha256":"%x"`, sum))
	assertions.NotContains(disclosure, media)
	_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": "stale", "patch": currentPatch})
	var refusal *mcpserver.OperationRefusalError
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("person_revision_conflict", refusal.Code)
	history, err := backend.ExecuteOperation(t.Context(), "get_person_record_history", map[string]any{"person_id": id})
	requirements.NoError(err)
	values := operationOutput[generated.PersonProfileHistory](t, history)
	requirements.Len(values.Addresses, 3)
	var oldIDs []int64
	for _, address := range values.Addresses {
		if address.Envelope.ActiveUntil != nil {
			requirements.Nil(address.Envelope.SupersededAt)
			oldIDs = append(oldIDs, address.Envelope.ID)
		}
	}
	requirements.Len(oldIDs, 2)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	args := map[string]any{"person_id": id, "etag": added.ETag, "patch": map[string]any{"addresses": map[string]any{"supersede": []int64{oldIDs[0]}}}}
	disclosure, err = backend.OperationDisclosure(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, "Old address")
	assertions.Contains(disclosure, `"active_until":"2020-01-01T00:00:00Z"`)
	patched, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_record", Arguments: args})
	requirements.NoError(err)
	requirements.False(patched.IsError, "%v", patched.StructuredContent)
	etag = loopToolOutput[mcpserver.StructuredPersonRecord](t, patched).ETag
	args["etag"] = etag
	_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", args)
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("profile_value_not_found", refusal.Code)
	args["patch"] = map[string]any{"addresses": map[string]any{"supersede": []int64{oldIDs[1]}}}
	armed.Store(true)
	_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", args)
	select {
	case err := <-result:
		requirements.NoError(err)
	case <-time.After(5 * time.Second):
		requirements.FailNow("history read did not trigger the concurrent patch")
	}
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("person_revision_conflict", refusal.Code)
}

func TestPersonRecordSDKPatchAllKinds(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, id, _, _ := personRecordFixture(t)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
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
	updated := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", args))
	assertions.NotEqual(current.ETag, updated.ETag)
	assertions.Len(updated.Profile.Names, 1)
	assertions.Len(updated.Profile.ContactPoints, 1)
	assertions.Len(updated.Profile.Addresses, 1)
	assertions.Len(updated.Profile.Dates, 1)
	assertions.Len(updated.Profile.Categories, 1)
	assertions.Len(updated.Profile.Media, 1)
}

func TestPersonRecordCapabilitiesAndRefusals(t *testing.T) {
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
			if !tc.removeProperty {
				copyCaps.Routes = slices.DeleteFunc(copyCaps.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == tc.operation })
			}
			for i := range copyCaps.Routes {
				if copyCaps.Routes[i].OperationID == tc.operation && tc.removeProperty {
					copyCaps.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(copyCaps.Routes[i].RequestProperties), func(s string) bool { return s == "media" })
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
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	patch := map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "uri": "https://example.test/image", "envelope": map[string]any{"source": "user"}}}}}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": result.ETag, "patch": patch})
	requirements.NoError(err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	requirements.Len(updated.Profile.Media, 1)
	for _, tc := range []struct {
		name string
		args map[string]any
		code string
	}{
		{"get_person_record", map[string]any{"person_id": 0}, "invalid_arguments"},
		{"get_person_record_media", map[string]any{"person_id": id, "media_id": 99999999}, "profile_media_not_found"},
		{"patch_person_record", map[string]any{"person_id": id, "etag": "x"}, "invalid_arguments"},
		{"get_person_record_media", map[string]any{"person_id": id, "media_id": updated.Profile.Media[0].Envelope.ID}, "profile_media_content_unavailable"},
		{"patch_person_record", map[string]any{"person_id": id, "etag": "*", "patch": patch}, "invalid_if_match"},
		{"patch_person_record", map[string]any{"person_id": id, "etag": updated.ETag, "patch": map[string]any{"categories": map[string]any{"add": []any{map[string]any{"original_value": "Friends", "envelope": map[string]any{"source": "invalid"}}}}}}, "invalid_profile_value"},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		requirements.NoError(err)
		assertLoopRefusal(t, result, tc.code, false)
	}
}

func TestPersonRecordLargeInlineMedia(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, reads, _ := personRecordFixture(t)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, nil)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	data := []byte(strings.Repeat("x", 3<<20))
	args := map[string]any{"person_id": id, "etag": result.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(data), "envelope": map[string]any{"source": "user"}}}}}}
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
	assertions.Equal("image/png", media.MediaType)
	assertions.Equal(int64(1), reads.Load())
}

func TestPersonRecordLargeProfileRecoveryAndDisclosure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, _, _ := personRecordFixture(t)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
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
	for batch := range 2 {
		adds := make([]any, 200)
		for i := range adds {
			adds[i] = map[string]any{"address_kind": "postal", "free_text": fmt.Sprintf("address-%d-%d-%s", batch, i, strings.Repeat("x", 3000)), "envelope": map[string]any{"source": "user"}}
		}
		result := call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"addresses": map[string]any{"add": adds}}})
		record := loopToolOutput[mcpserver.StructuredPersonRecord](t, result)
		etag = record.ETag
	}
	var assembled bytes.Buffer
	args := map[string]any{"person_id": id}
	for {
		chunk := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", args))
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
	history := call("get_person_record_history", map[string]any{"person_id": id})
	assertions.Equal("operation_response_limit_exceeded", loopToolFailure(t, history)["error"])
	superseded := profile.Addresses[0]
	patch := map[string]any{"addresses": map[string]any{"supersede": []int64{superseded.Envelope.ID}}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": patch})
	requirements.NoError(err)
	updated := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": patch}))
	assertions.NotEqual(etag, updated.ETag)
	assertions.Contains(disclosure, *superseded.FreeText)
	assertions.NotContains(disclosure, *profile.Addresses[1].FreeText)
}

func TestPersonRecordSupersedeLargeValueAndDisclosureLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, _, patches := personRecordFixture(t)
	initial, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	value := strings.Repeat("x", 2<<20)
	approvals := 0
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	args := map[string]any{"person_id": id, "etag": initial.ETag, "patch": map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": value, "envelope": map[string]any{"source": "user"}}}}}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	assertions.GreaterOrEqual(strings.Index(disclosure, value), 0, "disclosure must include the full value")
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_record", Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError, "patch refused: %v", result.Content)
	current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	var profile generated.StructuredPersonProfile
	requirements.NoError(json.Unmarshal(current.Data, &profile))
	requirements.Len(profile.Addresses, 1)
	requirements.NotNil(profile.Addresses[0].FreeText)
	assertions.Zero(strings.Compare(*profile.Addresses[0].FreeText, value), "saved value must match the approved value")
	args = map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"addresses": map[string]any{"supersede": []int64{profile.Addresses[0].Envelope.ID}}}}
	disclosure, err = backend.OperationDisclosure(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	assertions.GreaterOrEqual(strings.Index(disclosure, value), 0, "disclosure must include the full superseded value")
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_record", Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError)

	etag := loopToolOutput[mcpserver.StructuredPersonRecord](t, result).ETag
	adds := make([]any, 16)
	for i := range adds {
		adds[i] = map[string]any{"address_kind": "postal", "free_text": strings.Repeat("y", 125000), "envelope": map[string]any{"source": "user"}}
	}
	seeded, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"addresses": map[string]any{"add": adds}}})
	requirements.NoError(err)
	requirements.False(seeded.IsError)
	current, err = backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(current.Data, &profile))
	requirements.Len(profile.Addresses, 16)
	ids := make([]int64, 0, 16)
	for _, address := range profile.Addresses {
		ids = append(ids, address.Envelope.ID)
	}
	patch := map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": strings.Repeat("z", 12000000), "envelope": map[string]any{"source": "user"}}}, "supersede": ids}}
	encoded, err := stdjson.Marshal(patch)
	requirements.NoError(err)
	requirements.Less(len(encoded), 12<<20)
	beforeApprovals, beforePatches := approvals, patches.Load()
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_record", Arguments: map[string]any{"person_id": id, "etag": current.ETag, "patch": patch}})
	requirements.NoError(err)
	assertions.Equal("operation_response_limit_exceeded", loopToolFailure(t, result)["error"])
	assertions.Equal(beforeApprovals, approvals)
	assertions.Equal(beforePatches, patches.Load())
}

func TestPersonRecordOversizeDisclosureRefused(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend, id, _, _ := personRecordFixture(t)
	adds := make([]any, 201)
	for i := range adds {
		adds[i] = map[string]any{"name_kind": "formatted", "formatted": "Example", "envelope": map[string]any{"source": "user"}}
	}
	initial, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	for _, tc := range []struct {
		patch map[string]any
		code  string
	}{
		{map[string]any{}, "invalid_profile_value"},
		{map[string]any{"names": map[string]any{"add": []any{}}}, "invalid_profile_value"},
		{map[string]any{"names": map[string]any{"add": adds}}, "profile_patch_too_large"},
		{map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": base64.StdEncoding.EncodeToString(make([]byte, (8<<20)+1)), "envelope": map[string]any{"source": "user"}}}}}, "invalid_profile_value"},
		{map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": base64.StdEncoding.EncodeToString(make([]byte, 8<<20)), "envelope": map[string]any{"source": "user"}}}}, "names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": strings.Repeat("<", 300000), "envelope": map[string]any{"source": "user"}}}}}, "profile_patch_too_large"},
	} {
		_, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": initial.ETag, "patch": tc.patch})
		var refusal *mcpserver.OperationRefusalError
		requirements.ErrorAs(err, &refusal)
		assertions.Equal(tc.code, refusal.Code)
	}
}
