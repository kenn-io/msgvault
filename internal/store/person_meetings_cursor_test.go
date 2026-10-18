package store

import (
	"encoding/base64"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func FuzzPersonMeetingsCursorRoundTrip(f *testing.F) {
	f.Add(int64(1800000000), int64(7))
	f.Fuzz(func(t *testing.T, unix, id int64) {
		if unix < 0 || unix > 4102444800 || id <= 0 || id > 9007199254740991 {
			return
		}
		now := time.Unix(unix, 0).UTC()
		q := PersonMeetingsQuery{ParticipantIDs: []int64{id}, Emails: []string{"Guest@example.com"}, Limit: 1}
		first, _, key, err := normalizePersonMeetingsQuery(OwnerScope(), q, now)
		require.NoError(t, err)
		encoded, err := json.Marshal(personMeetingsCursor{From: first.From, To: first.To, Start: first.From.Add(time.Hour), Ref: "msg:1", Query: key})
		require.NoError(t, err)
		q.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
		next, _, nextKey, err := normalizePersonMeetingsQuery(OwnerScope(), q, now.Add(24*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, first.From, next.From)
		assert.Equal(t, first.To, next.To)
		assert.Equal(t, key, nextKey)
	})
}
