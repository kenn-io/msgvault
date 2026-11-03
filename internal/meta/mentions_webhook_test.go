package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeMentionChange(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        MentionEvent
	}{
		{"caption", `{"media_id":"17900000000000001"}`, MentionEvent{MediaID: "17900000000000001"}},
		{"comment", `{"media_id":"17900000000000001","comment_id":"18000000000000001"}`, MentionEvent{MediaID: "17900000000000001", CommentID: "18000000000000001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeMentionChange("17841400000000001", []byte(tc.value))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
func TestDecodeMentionChangeRefuses(t *testing.T) {
	for _, value := range []string{`{"media_id":"123","comment_id":""}`, `{"media_id":"123","comment_id":null}`, `{}`, `null`, `[]`, `1`, `{"media_id":"abc"}`, `{"media_id":"123","comment_id":"abc"}`, `{"media_id":"123","extra":1}`, `{"media_id":123}`, `{"media_id":"123","media_id":"456"}`, `{"media_id":"123"} {}`} {
		t.Run(value, func(t *testing.T) {
			_, err := DecodeMentionChange("17841400000000001", []byte(value))
			require.Error(t, err)
		})
	}
	_, err := DecodeMentionChange("not-an-id", []byte(`{"media_id":"123"}`))
	require.Error(t, err)
}
