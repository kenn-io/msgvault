package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personRecordFixture(t *testing.T) (*daemonMCPOperations, int64) {
	t.Helper()
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("synthetic@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	return sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)})), person.ID
}

func TestPersonRecordSDKAtomicPatchAndHistory(t *testing.T) {
	backend, id := personRecordFixture(t)
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
		require.NoError(t, err)
		return result
	}
	current := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id}))
	require.NotEmpty(t, current.ETag)
	patch := map[string]any{
		"names":          map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Example Person", "envelope": map[string]any{"source": "user"}}}},
		"contact_points": map[string]any{"add": []any{map[string]any{"address_kind": "email", "original_value": "person@example.test", "envelope": map[string]any{"source": "user"}}}},
		"addresses":      map[string]any{"add": []any{map[string]any{"address_kind": "postal", "free_text": "Exampleville", "envelope": map[string]any{"source": "user"}}}},
		"dates":          map[string]any{"add": []any{map[string]any{"date_kind": "custom", "date_text": "Spring 2020", "envelope": map[string]any{"source": "user"}}}},
		"categories":     map[string]any{"add": []any{map[string]any{"original_value": "Friends", "envelope": map[string]any{"source": "user"}}}},
		"media":          map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("synthetic")), "envelope": map[string]any{"source": "user"}}}},
	}
	args := map[string]any{"person_id": id, "etag": current.ETag, "patch": patch}
	assert.True(t, call("patch_person_record", args).IsError)
	unchanged := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("get_person_record", map[string]any{"person_id": id}))
	assert.Equal(t, current.ETag, unchanged.ETag)
	approved = true
	updated := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", args))
	assert.NotEqual(t, current.ETag, updated.ETag)
	assert.Contains(t, disclosure, "person@example.test")
	sum := sha256.Sum256([]byte("synthetic"))
	assert.Contains(t, disclosure, `"data_size":9`)
	assert.Contains(t, disclosure, fmt.Sprintf(`"data_sha256":"%x"`, sum))
	assert.NotContains(t, disclosure, base64.StdEncoding.EncodeToString([]byte("synthetic")))
	assert.Len(t, updated.Profile.Names, 1)
	assert.Len(t, updated.Profile.ContactPoints, 1)
	assert.Len(t, updated.Profile.Addresses, 1)
	assert.Len(t, updated.Profile.Dates, 1)
	assert.Len(t, updated.Profile.Categories, 1)
	require.Len(t, updated.Profile.Media, 1)
	mediaID := updated.Profile.Media[0].Envelope.ID
	media := loopToolOutput[mcpserver.PersonMediaContent](t, call("get_person_record_media", map[string]any{"person_id": id, "media_id": mediaID}))
	assert.Equal(t, "image/png", media.MediaType)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("synthetic")), media.DataBase64)
	assert.True(t, media.Complete)
	history := loopToolOutput[generated.PersonProfileHistory](t, call("get_person_record_history", map[string]any{"person_id": id}))
	assert.Len(t, history.Categories, 1)
	assert.Equal(t, "person_revision_conflict", loopToolFailure(t, call("patch_person_record", args))["error"])
	closeArgs := map[string]any{"person_id": id, "etag": updated.ETag, "patch": map[string]any{"categories": map[string]any{"supersede": []int64{updated.Profile.Categories[0].Envelope.ID}}}}
	closed := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", closeArgs))
	assert.Empty(t, closed.Profile.Categories)
}

func TestPersonRecordCapabilitiesAndRefusals(t *testing.T) {
	backend, id := personRecordFixture(t)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct {
		name, operation string
		removeProperty  bool
	}{
		{"get_person_record_history", "getPersonProfileHistory", false}, {"patch_person_record", "patchPersonStructuredProfile", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			assert.NotContains(t, got, tc.name)
			if tc.removeProperty {
				assert.Contains(t, got, "get_person_record")
				assert.Contains(t, got, "get_person_record_history")
				assert.Contains(t, got, "get_person_record_media")
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
		require.NoError(t, err)
		assertLoopRefusal(t, result, tc.code, false)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	require.NoError(t, err)
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	invalid := map[string]any{"person_id": id, "etag": record.ETag, "patch": map[string]any{
		"names":      map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Valid Name", "envelope": map[string]any{"source": "user"}}}},
		"categories": map[string]any{"add": []any{map[string]any{"original_value": "Friends", "envelope": map[string]any{"source": "invalid"}}}},
	}}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", invalid)
	require.NoError(t, err)
	assertLoopRefusal(t, result, "invalid_profile_value", false)
	after, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	require.NoError(t, err)
	unchanged := operationOutput[mcpserver.StructuredPersonRecord](t, after)
	assert.Equal(t, record.ETag, unchanged.ETag)
}

func TestPersonRecordLargeInlineMedia(t *testing.T) {
	backend, id := personRecordFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	require.NoError(t, err)
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	data := []byte(strings.Repeat("x", 8<<20))
	args := map[string]any{"person_id": id, "etag": record.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(data), "envelope": map[string]any{"source": "user"}}}}}}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	require.NoError(t, err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	require.Len(t, updated.Profile.Media, 1)
	result, err = backend.ExecuteOperation(t.Context(), "get_person_record_media", map[string]any{"person_id": id, "media_id": updated.Profile.Media[0].Envelope.ID})
	require.NoError(t, err)
	media := operationOutput[mcpserver.PersonMediaContent](t, result)
	assert.Equal(t, "application/octet-stream", media.MediaType)
	sum := sha256.Sum256(data)
	assert.Equal(t, int64(1<<20), media.Length)
	assert.Equal(t, int64(8<<20), media.Size)
	assert.Equal(t, fmt.Sprintf("%x", sum), media.SHA256)
	assert.False(t, media.Complete)
	var assembled bytes.Buffer
	for i := 0; i < 8; i++ {
		assert.Equal(t, int64(i<<20), media.Offset)
		decoded, err := base64.StdEncoding.DecodeString(media.DataBase64)
		require.NoError(t, err)
		_, err = assembled.Write(decoded)
		require.NoError(t, err)
		if media.Complete {
			break
		}
		result, err = backend.ExecuteOperation(t.Context(), "get_person_record_media", map[string]any{"person_id": id, "media_id": updated.Profile.Media[0].Envelope.ID, "offset": float64(media.Offset + media.Length), "sha256": media.SHA256})
		require.NoError(t, err)
		media = operationOutput[mcpserver.PersonMediaContent](t, result)
	}
	assert.True(t, media.Complete)
	assert.Equal(t, data, assembled.Bytes())
}

func TestPersonRecordURIOnlyAndRevisionRefusals(t *testing.T) {
	backend, id := personRecordFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	require.NoError(t, err)
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	patch := map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "uri": "https://example.test/image", "envelope": map[string]any{"source": "user"}}}}}
	args := map[string]any{"person_id": id, "etag": record.ETag, "patch": patch}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	require.NoError(t, err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	require.Len(t, updated.Profile.Media, 1)
	mediaID := updated.Profile.Media[0].Envelope.ID
	result, err = backend.ExecuteOperation(t.Context(), "get_person_record_media", map[string]any{"person_id": id, "media_id": mediaID})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "profile_media_content_unavailable", false)
	args["etag"] = "*"
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	require.NoError(t, err)
	assertLoopRefusal(t, result, "invalid_if_match", false)
}
