package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMessageHeaderBlockContext(t *testing.T) {
	f := storetest.New(t)
	for _, tc := range []struct{ name, raw, want string }{
		{"zlib MIME", "From: sender@example.test\r\nSubject: Test\r\n\r\nprivate body", "From: sender@example.test\r\nSubject: Test\r\n\r\n"},
		{"header only", "From: sender@example.test", "From: sender@example.test\r\n\r\n"},
		{"no raw", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := f.CreateMessage(tc.name)
			if tc.raw != "" {
				require.NoError(t, f.Store.UpsertMessageRaw(id, []byte(tc.raw)))
			}
			block, err := f.Store.MessageHeaderBlockContext(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(block))
		})
	}
}
