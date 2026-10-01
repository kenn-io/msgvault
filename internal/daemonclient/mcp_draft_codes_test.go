package daemonclient

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestMCPCLIRecentDraftRefusalsKeepFixedCodes(t *testing.T) {
	for _, code := range []string{"attachment_preflight_failed", "chat_not_found", "draft_exists", "invalid_destination", "invalid_forward_metadata", "local_store_failed", "provider_identity_mismatch", "provider_rejected", "provider_unavailable", "unsupported_source"} {
		result, err := decodeMCPCLIStream(strings.NewReader(fmt.Sprintf(`{"type":"error","error":%q}`+"\n", code)))
		require.Error(t, err)
		assert.Equal(t, code, result.ErrorCode)
	}
	result, err := decodeMCPCLIStream(strings.NewReader(`{"type":"error","error":"private host diagnostic"}` + "\n"))
	require.Error(t, err)
	assert.Equal(t, "cli_execution_failed", result.ErrorCode)
}
