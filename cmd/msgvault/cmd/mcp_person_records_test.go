package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	assertions := assert.New(t)
	requirements := require.New(t)
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
	history := loopToolOutput[generated.PersonProfileHistory](t, call("get_person_record_history", map[string]any{"person_id": id}))
	assertions.Len(history.Categories, 1)
	assertions.Equal("person_revision_conflict", loopToolFailure(t, call("patch_person_record", args))["error"])
	closeArgs := map[string]any{"person_id": id, "etag": updated.ETag, "patch": map[string]any{"categories": map[string]any{"supersede": []int64{updated.Profile.Categories[0].Envelope.ID}}}}
	closed := loopToolOutput[mcpserver.StructuredPersonRecord](t, call("patch_person_record", closeArgs))
	assertions.Empty(closed.Profile.Categories)
}

func TestPersonRecordCapabilitiesAndRefusals(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, id := personRecordFixture(t)
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
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
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
	unchanged := operationOutput[mcpserver.StructuredPersonRecord](t, after)
	assertions.Equal(record.ETag, unchanged.ETag)
}

func TestPersonRecordLargeInlineMedia(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, id := personRecordFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	data := []byte(strings.Repeat("x", 8<<20))
	args := map[string]any{"person_id": id, "etag": record.ETag, "patch": map[string]any{"media": map[string]any{"add": []any{map[string]any{"media_kind": "photo", "media_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(data), "envelope": map[string]any{"source": "user"}}}}}}
	result, err = backend.ExecuteOperation(t.Context(), "patch_person_record", args)
	requirements.NoError(err)
	updated := operationOutput[mcpserver.StructuredPersonRecord](t, result)
	requirements.Len(updated.Profile.Media, 1)
	result, err = backend.ExecuteOperation(t.Context(), "get_person_record_media", map[string]any{"person_id": id, "media_id": updated.Profile.Media[0].Envelope.ID})
	requirements.NoError(err)
	media := operationOutput[mcpserver.PersonMediaContent](t, result)
	assertions.Equal("application/octet-stream", media.MediaType)
	sum := sha256.Sum256(data)
	assertions.Equal(int64(1<<20), media.Length)
	assertions.Equal(int64(8<<20), media.Size)
	assertions.Equal(hex.EncodeToString(sum[:]), media.SHA256)
	assertions.False(media.Complete)
	var assembled bytes.Buffer
	for i := range 8 {
		assertions.Equal(int64(i<<20), media.Offset)
		decoded, err := base64.StdEncoding.DecodeString(media.DataBase64)
		requirements.NoError(err)
		_, err = assembled.Write(decoded)
		requirements.NoError(err)
		if media.Complete {
			break
		}
		result, err = backend.ExecuteOperation(t.Context(), "get_person_record_media", map[string]any{"person_id": id, "media_id": updated.Profile.Media[0].Envelope.ID, "offset": float64(media.Offset + media.Length), "sha256": media.SHA256})
		requirements.NoError(err)
		media = operationOutput[mcpserver.PersonMediaContent](t, result)
	}
	assertions.True(media.Complete)
	assertions.Equal(data, assembled.Bytes())
}

func TestPersonRecordURIOnlyAndRevisionRefusals(t *testing.T) {
	requirements := require.New(t)
	backend, id := personRecordFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
	requirements.NoError(err)
	record := operationOutput[mcpserver.StructuredPersonRecord](t, result)
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
