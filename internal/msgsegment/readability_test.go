package msgsegment

import (
	"github.com/stretchr/testify/assert"
	"strings"
	"testing"
)

func TestReadableTextPreservesDisplayRules(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"Reply\r\n> old\r\n\r\nAnswer", "Reply\n\nAnswer"},
		{"Reply\nOn Tue, 3 Mar 2026, A wrote:\n> Old", "Reply"},
		{"Reply\nOn Tue, 3 Mar 2026,\nA wrote:\n> Old", "Reply\nOn Tue, 3 Mar 2026,\nA wrote:"},
		{"```\n> code\n```", "```\n```"},
		{"New\n-- \r", "New\n--"},
		{"New\nBegin forwarded message:\nFrom: A\nDate: Monday\nOld", "New\nBegin forwarded message:\nFrom: A\nDate: Monday\nOld"},
	} {
		assert.Equal(t, tc.want, ReadableText(tc.body))
	}
	// Display cleanup historically processes the complete body, beyond the
	// segmentation readout's cap. Keep hiding quotes in that tail too.
	prefix := strings.Repeat("a", MaxBodyBytes+1)
	assert.Equal(t, prefix, ReadableText(prefix+"\n> tail"))
}
