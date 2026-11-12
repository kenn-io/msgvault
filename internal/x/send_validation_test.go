package x

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestXReplyTextTransportValidation(t *testing.T) {
	for _, text := range []string{"thanks", "你好 👋 https://example.com", strings.Repeat("a", 16*1024), strings.Repeat("😀", 4096)} {
		require.NoError(t, ValidateReplyText(text))
	}
	for _, text := range []string{"", " \n\t\u2003", "hello\x00world", string([]byte{0xff}), strings.Repeat("a", 16*1024+1)} {
		require.Error(t, ValidateReplyText(text))
	}
}
