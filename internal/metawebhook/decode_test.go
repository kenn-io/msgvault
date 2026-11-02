package metawebhook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodePageFeedChanges(t *testing.T) {
	body := []byte(`{"object":"page","entry":[{"id":"100","time":123,"changes":[{"field":"feed","value":{"item":"comment","comment_id":"100_200_300"}}]}]}`)
	got, err := Decode("9", body)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, Item{Index: 0, AppID: "9", Object: "page", EntryID: "100", Time: 123, Field: "feed", Value: []byte(`{"item":"comment","comment_id":"100_200_300"}`)}, got[0])
	assert.Empty(t, got[0].DeliveryID)
}
func TestDecodeMessengerMessaging(t *testing.T) {
	for _, tt := range []struct{ value, field string }{
		{`{"message":{"mid":"m1"}}`, "messages"}, {`{"message":{"mid":"m2","is_echo":true}}`, "message_echoes"}, {`{"reaction":{}}`, "message_reactions"},
		{`{"message_edit":{}}`, "message_edits"}, {`{"read":{}}`, "message_reads"}, {`{"delivery":{}}`, "message_deliveries"}, {`{"postback":{}}`, "messaging_postbacks"},
		{`{"referral":{}}`, "messaging_referrals"}, {`{"optin":{}}`, "messaging_optins"}, {`{"pass_thread_control":{}}`, "messaging_handovers"}, {`{"take_thread_control":{}}`, "messaging_handovers"},
		{`{"request_thread_control":{}}`, "messaging_handovers"}, {`{"app_roles":[]}`, "messaging_handovers"}, {`{"unknown":{}}`, "messaging_unknown"},
		{`{"message":{"mid":"m3"},"referral":{}}`, "messages"},
	} {
		t.Run(tt.field+tt.value, func(t *testing.T) {
			got, err := Decode("9", []byte(fmt.Sprintf(`{"object":"page","entry":[{"id":"100","messaging":[%s]}]}`, tt.value)))
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, tt.field, got[0].Field)
			assert.JSONEq(t, tt.value, string(got[0].Value))
		})
	}
	got, err := Decode("9", []byte(`{"object":"page","entry":[{"id":"100","standby":[{"message":{"mid":"m1"}}]}]}`))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "standby", got[0].Field)
}
func TestDecodeInstagramMessagingFields(t *testing.T) {
	for _, tt := range []struct{ member, field string }{{`"message":{"is_echo":true}`, "messages"}, {`"read":{}`, "messaging_seen"}, {`"referral":{}`, "messaging_referral"}, {`"reaction":{}`, "message_reactions"}} {
		items, err := Decode("9", []byte(fmt.Sprintf(`{"object":"instagram","entry":[{"id":"200","messaging":[{%s}]}]}`, tt.member)))
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, tt.field, items[0].Field)
	}
}
func TestDecodeInstagramComments(t *testing.T) {
	raw := `{"from":{"id":"300","username":"example_actor"},"comment_id":"400","parent_id":"401","text":"Synthetic comment","media":{"id":"500","ad_id":"600","ad_title":"Synthetic ad","original_media_id":"501","media_product_type":"FEED"}}`
	items, err := Decode("9", []byte(`{"object":"instagram","entry":[{"id":"200","time":123,"changes":[{"field":"comments","value":`+raw+`}]}]}`))
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "comments", items[0].Field)
	assert.JSONEq(t, raw, string(items[0].Value))
}
func TestDecodeUnknownObjectInvalid(t *testing.T) {
	for _, body := range []string{`{"object":"unknown","entry":[]}`, `invalid`, `{}`} {
		_, err := Decode("9", []byte(body))
		assert.Error(t, err)
	}
}
func TestDecodeBatch1000(t *testing.T) {
	entry := `{"id":"100","changes":[{"field":"feed","value":{}}]}`
	body := `{"object":"page","entry":[` + strings.Repeat(entry+",", 999) + entry + `]}`
	got, err := Decode("9", []byte(body))
	require.NoError(t, err)
	require.Len(t, got, 1000)
	assert.Equal(t, 999, got[999].Index)
	body = `{"object":"page","entry":[` + strings.Repeat(entry+",", 5000) + entry + `]}`
	_, err = Decode("9", []byte(body))
	assert.Error(t, err)
}
