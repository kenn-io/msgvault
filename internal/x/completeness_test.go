package x

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientLongTextCompletenessRequiresUsableFullText(t *testing.T) {
	for _, tc := range []struct {
		name, note, want string
		incomplete       bool
	}{
		{name: "omitted", incomplete: true},
		{name: "null", note: `,"note_tweet":null`, incomplete: true},
		{name: "empty object", note: `,"note_post":{}`, incomplete: true},
		{name: "null text", note: `,"note_tweet":{"text":null}`, incomplete: true},
		{name: "empty text", note: `,"note_post":{"text":""}`, incomplete: true},
		{name: "full text", note: `,"note_tweet":{"text":"Full synthetic text"}`, want: "Full synthetic text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			post := strings.TrimSuffix(rootJSON, "}") + tc.note + "}"
			client, calls := servePage(t, `{"data":[`+post+`]}`)
			page, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			_testRequire.NoError(err)
			_testRequire.Len(page.Posts, 1)
			_testAssert.Equal(tc.want, page.Posts[0].LongText)
			_testAssert.Equal(tc.incomplete, page.Posts[0].TextIncomplete)
			_testAssert.Equal(int64(1), calls.Load())
		})
	}
}
