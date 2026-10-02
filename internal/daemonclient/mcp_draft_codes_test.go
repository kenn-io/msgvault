package daemonclient

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPCLIRecentDraftRefusalsKeepFixedCodes(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, code := range []string{"attachment_preflight_failed", "chat_not_found", "draft_exists", "invalid_destination", "invalid_forward_metadata", "local_store_failed", "provider_identity_mismatch", "provider_rejected", "provider_unavailable", "unsupported_source"} {
		result, err := decodeMCPCLIStream(strings.NewReader(fmt.Sprintf(`{"type":"error","error":%q}`+"\n", code)))
		requirements.Error(err)
		assertions.Equal(code, result.ErrorCode)
	}
	result, err := decodeMCPCLIStream(strings.NewReader(`{"type":"error","error":"private host diagnostic"}` + "\n"))
	requirements.Error(err)
	assertions.Equal("cli_execution_failed", result.ErrorCode)
}
