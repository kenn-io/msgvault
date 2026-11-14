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
	"strconv"
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

func personRecordFixture(t *testing.T) (*daemonMCPOperations, int64) {
	t.Helper()
	return personRecordFixtureWithHook(t, nil)
}

func personRecordFixtureWithHook(t *testing.T, hook func(*http.Request)) (*daemonMCPOperations, int64) {
	t.Helper()
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("synthetic@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}).Router()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(r)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	return newDaemonMCPOperations(client, caps), person.ID
}

func TestPersonRecordDisclosure(t *testing.T) {
	t.Run("refusals", func(t *testing.T) {
		backend, id := personRecordFixture(t)
		adds := make([]any, 201)
		for i := range adds {
			adds[i] = map[string]any{"name_kind": "formatted", "formatted": "Example", "envelope": map[string]any{"source": "user"}}
		}
		for i, tc := range []struct {
			patch    map[string]any
			code     string
			personID int64
		}{
			{map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Example", "envelope": map[string]any{"source": "user"}}}}}, "person_revision_conflict", 0},
			{nil, "invalid_arguments", 0},
			{map[string]any{}, "invalid_profile_value", 0},
			{map[string]any{"names": map[string]any{"add": adds}}, "profile_patch_too_large", 0},
			{map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": base64.StdEncoding.EncodeToString(make([]byte, (8<<20)+1)), "envelope": map[string]any{"source": "user"}}}}}, "invalid_profile_value", 0},
			{map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": strings.Repeat("x", (12<<20)+1), "envelope": map[string]any{"source": "user"}}}}}, "profile_patch_too_large", 0},
			{map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Example", "envelope": map[string]any{"source": "user"}}}}}, "person_profile_not_found", 999999},
		} {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				args := map[string]any{"person_id": id, "etag": "etag"}
				if tc.personID != 0 {
					args["person_id"] = tc.personID
				}
				if tc.patch != nil {
					args["patch"] = tc.patch
				}
				_, err := backend.OperationDisclosure(t.Context(), "patch_person_record", args)
				var refusal *mcpserver.OperationRefusalError
				require.ErrorAs(t, err, &refusal)
				assert.Equal(t, tc.code, refusal.Code)
			})
		}
	})
	t.Run("full value", func(t *testing.T) {
		backend, id := personRecordFixture(t)
		current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		value := strings.Repeat("x", 2<<20)
		patch := map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": value, "envelope": map[string]any{"source": "user"}}}}}
		disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": patch})
		require.NoError(t, err)
		assert.Contains(t, disclosure, value)
	})
	t.Run("current value selection", func(t *testing.T) {
		var historyReads atomic.Int64
		backend, id := personRecordFixtureWithHook(t, func(r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/profile/history") {
				historyReads.Add(1)
			}
		})
		current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		added, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"addresses": map[string]any{"add": []any{
			map[string]any{"address_kind": "postal", "free_text": "Current address", "envelope": map[string]any{"source": "user"}},
			map[string]any{"address_kind": "postal", "free_text": "Other current address", "envelope": map[string]any{"source": "user"}},
		}}}})
		require.NoError(t, err)
		profile := operationOutput[mcpserver.StructuredPersonRecord](t, added)
		require.Len(t, profile.Profile.Addresses, 2)
		patch := map[string]any{"addresses": map[string]any{"supersede": []int64{profile.Profile.Addresses[0].Envelope.ID}}}
		disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": added.ETag, "patch": patch})
		require.NoError(t, err)
		assert.Contains(t, disclosure, "Current address")
		assert.NotContains(t, disclosure, "Other current address")
		assert.Zero(t, historyReads.Load())
	})
	t.Run("media redaction", func(t *testing.T) {
		backend, id := personRecordFixture(t)
		current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		media := base64.StdEncoding.EncodeToString([]byte("synthetic"))
		disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "data": media, "envelope": map[string]any{"source": "user"}}}}}})
		require.NoError(t, err)
		sum := sha256.Sum256([]byte("synthetic"))
		assert.Contains(t, disclosure, `"data_size":9`)
		assert.Contains(t, disclosure, fmt.Sprintf(`"data_sha256":"%x"`, sum))
		assert.NotContains(t, disclosure, media)
	})
	t.Run("history supersede and concurrent revision", func(t *testing.T) {
		var armed atomic.Bool
		var backend *daemonMCPOperations
		var id int64
		var etag string
		type patchResult struct {
			etag string
			err  error
		}
		result := make(chan patchResult, 1)
		backend, id = personRecordFixtureWithHook(t, func(r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/profile/history") && armed.CompareAndSwap(true, false) {
				updated, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Updated", "envelope": map[string]any{"source": "user"}}}}}})
				if err == nil && updated.IsError {
					err = fmt.Errorf("concurrent patch refused: %v", updated.Output)
				}
				var newETag string
				if updated != nil {
					newETag = updated.ETag
				}
				result <- patchResult{newETag, err}
			}
		})
		current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		added, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": "Old address", "envelope": map[string]any{"source": "user", "active_until": "2020-01-01T00:00:00Z"}}}}}})
		require.NoError(t, err)
		history, err := backend.ExecuteOperation(t.Context(), "get_person_record_history", map[string]any{"person_id": id})
		require.NoError(t, err)
		values := operationOutput[generated.PersonProfileHistory](t, history)
		require.Len(t, values.Addresses, 1)
		require.Nil(t, values.Addresses[0].Envelope.SupersededAt)
		args := map[string]any{"person_id": id, "etag": added.ETag, "patch": map[string]any{"addresses": map[string]any{"supersede": []int64{values.Addresses[0].Envelope.ID}}}}
		etag = added.ETag
		armed.Store(true)
		_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", args)
		select {
		case updated := <-result:
			require.NoError(t, updated.err)
			etag = updated.etag
		case <-time.After(5 * time.Second):
			require.FailNow(t, "history read did not trigger the concurrent patch")
		}
		var refusal *mcpserver.OperationRefusalError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "person_revision_conflict", refusal.Code)
		args["etag"] = etag
		disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_record", args)
		require.NoError(t, err)
		assert.Contains(t, disclosure, "Old address")
		assert.Contains(t, disclosure, `"active_until":"2020-01-01T00:00:00Z"`)
		patched, err := backend.ExecuteOperation(t.Context(), "patch_person_record", args)
		require.NoError(t, err)
		require.False(t, patched.IsError)
		args["etag"] = patched.ETag
		_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", args)
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "profile_value_not_found", refusal.Code)
	})
	t.Run("disclosure limit", func(t *testing.T) {
		backend, id := personRecordFixture(t)
		current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		adds := make([]any, 16)
		for i := range adds {
			adds[i] = map[string]any{"address_kind": "postal", "free_text": strings.Repeat("y", 125000), "envelope": map[string]any{"source": "user"}}
		}
		seeded, err := backend.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": map[string]any{"addresses": map[string]any{"add": adds}}})
		require.NoError(t, err)
		require.False(t, seeded.IsError)
		current, err = backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		var profile generated.StructuredPersonProfile
		require.NoError(t, json.Unmarshal(current.Data, &profile))
		require.Len(t, profile.Addresses, 16)
		ids := make([]int64, 0, 16)
		for _, address := range profile.Addresses {
			ids = append(ids, address.Envelope.ID)
		}
		patch := map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": strings.Repeat("z", 12000000), "envelope": map[string]any{"source": "user"}}}, "supersede": ids}}
		encoded, err := stdjson.Marshal(patch)
		require.NoError(t, err)
		require.Less(t, len(encoded), 12<<20)
		_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": current.ETag, "patch": patch})
		var refusal *mcpserver.OperationRefusalError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "operation_response_limit_exceeded", refusal.Code)
	})
}

func TestPersonRecordPatchAllKinds(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, id := personRecordFixture(t)
	current, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
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
	result, err := backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	assertions.NotEqual(current.ETag, updated.ETag)
	assertions.Len(updated.Profile.Names, 1)
	assertions.Len(updated.Profile.ContactPoints, 1)
	assertions.Len(updated.Profile.Addresses, 1)
	assertions.Len(updated.Profile.Dates, 1)
	assertions.Len(updated.Profile.Categories, 1)
	assertions.Len(updated.Profile.Media, 1)
}

func TestPersonRecordCapabilities(t *testing.T) {
	backend, _ := personRecordFixture(t)
	requirements := require.New(t)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	t.Run("missing history route", func(t *testing.T) {
		copyCaps := *capabilities
		copyCaps.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == "getPersonProfileHistory" })
		assert.NotContains(t, newDaemonMCPOperations(backend.client, &copyCaps).capabilities(), "get_person_record_history")
	})
	t.Run("missing media property", func(t *testing.T) {
		copyCaps := *capabilities
		copyCaps.Routes = slices.Clone(capabilities.Routes)
		for i := range copyCaps.Routes {
			if copyCaps.Routes[i].OperationID == "patchPersonStructuredProfile" {
				copyCaps.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(copyCaps.Routes[i].RequestProperties), func(s string) bool { return s == "media" })
			}
		}
		got := newDaemonMCPOperations(backend.client, &copyCaps).capabilities()
		assert.NotContains(t, got, "patch_person_record")
		assert.Contains(t, got, "get_person_record")
		assert.Contains(t, got, "get_person_record_history")
		assert.Contains(t, got, "get_person_record_media")
	})
}

func TestPersonRecordRefusals(t *testing.T) {
	backend, id := personRecordFixture(t)
	requirements := require.New(t)
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
		{"get_person_record_media", map[string]any{"person_id": id, "media_id": updated.Profile.Media[0].Envelope.ID}, "profile_media_content_unavailable"},
		{"patch_person_record", map[string]any{"person_id": id, "etag": "*", "patch": patch}, "invalid_if_match"},
		{"patch_person_record", map[string]any{"person_id": id, "etag": updated.ETag, "patch": map[string]any{"categories": map[string]any{"add": []any{map[string]any{"original_value": "Friends", "envelope": map[string]any{"source": "invalid"}}}}}}, "invalid_profile_value"},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		requirements.NoError(err)
		assertLoopRefusal(t, result, tc.code, false)
	}
}

func TestPersonRecordLargeProfileAndSmallMedia(t *testing.T) {
	backend, id := personRecordFixture(t)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyCurated}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	t.Run("media", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		requirements.NoError(err)
		data := []byte("synthetic media")
		args := map[string]any{"person_id": id, "etag": result.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(data), "envelope": map[string]any{"source": "user"}}}}}}
		result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
		requirements.NoError(err)
		updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
		requirements.Len(updated.Profile.Media, 1)
		mediaID := updated.Profile.Media[0].Envelope.ID
		resultMedia, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_record_media", Arguments: map[string]any{"person_id": id, "media_id": mediaID}})
		requirements.NoError(err)
		media := loopToolOutput[mcpserver.PersonMediaContent](t, resultMedia)
		decoded, err := base64.StdEncoding.DecodeString(media.DataBase64)
		requirements.NoError(err)
		assertions.Equal(data, decoded)
		assertions.Equal("image/png", media.MediaType)
	})
	t.Run("profile", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
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
		updated := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", map[string]any{"person_id": id, "etag": etag, "patch": patch}))
		assertions.NotEqual(etag, updated.ETag)
		assertions.Nil(updated.Profile)
	})
}
