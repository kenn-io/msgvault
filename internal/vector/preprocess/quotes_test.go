package preprocess

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPreprocessWrappedAndIndentedQuotes(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"wrapped attribution with reply below", "Thanks!\n\nOn Monday,\nAlex Example wrote:\n  > Can you send it?\n\nThe report is attached.", "Thanks!\n\n\nThe report is attached."},
		{"indented quotation", "Received.\n  > Can you send it?", "Received."},
		{"unmarked quoted tail", "Thanks!\n\n On Monday, Alex wrote:\nCan you send it?", "Thanks!"},
		{"forwarded tail", "Received.\n\n-----Original Message-----\nPlease send it.", "Received."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Preprocess("", tc.body, 0, Config{StripQuotes: true})
			assert.Equal(t, tc.want, got)
		})
	}
}
