package whatsapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextImportOmittedMedia(t *testing.T) {
	for _, tc := range []struct {
		body    string
		omitted int
	}{
		{"\u200eimage omitted", 1},
		{"<Media omitted>", 1},
		{"image omitted from the list", 0},
		{"image omitted", 0},
		{"asters \u200eimage omitted", 1},
		{"\u200eContact card omitted", 1},
		{"asters \u200eimage omitted \u200e<This message was edited>", 1},
	} {
		t.Run(tc.body, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			opts := textOptions()
			opts.DateOrder = "mdy"
			input := "[6/13/2026, 10:00:01 AM] Mira: " + tc.body + "\r\n"
			summary, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, input), opts)
			require.NoError(err)
			assert.Equal(tc.omitted, summary.MediaOmitted)
			assert.Zero(summary.MediaSkipped)
		})
	}
}
