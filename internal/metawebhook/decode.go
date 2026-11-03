package metawebhook

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

const maxDeliveryItems = 5000

type Item struct {
	ReceiptOrder                              uint64
	Index                                     int
	DeliveryID, AppID, Object, EntryID, Field string
	Time                                      int64
	Value                                     jsontext.Value
}

// Decode preserves each raw event; field selection does not apply its content.
func Decode(appID string, body []byte) ([]Item, error) {
	var envelope struct {
		Object string `json:"object"`
		Entry  []struct {
			ID      string `json:"id"`
			Time    int64  `json:"time"`
			Changes []struct {
				Field string         `json:"field"`
				Value jsontext.Value `json:"value"`
			} `json:"changes"`
			Messaging []jsontext.Value `json:"messaging"`
			Standby   []jsontext.Value `json:"standby"`
		} `json:"entry"`
	}
	if json.Unmarshal(body, &envelope) != nil || (envelope.Object != "page" && envelope.Object != "instagram") || envelope.Entry == nil {
		return nil, errors.New("invalid meta webhook envelope")
	}
	items := []Item{}
	for _, entry := range envelope.Entry {
		add := func(field string, value jsontext.Value) {
			items = append(items, Item{Index: len(items), AppID: appID, Object: envelope.Object, EntryID: entry.ID, Time: entry.Time, Field: field, Value: value})
		}
		for _, change := range entry.Changes {
			add(change.Field, change.Value)
		}
		for _, value := range entry.Messaging {
			add(messagingField(envelope.Object, value), value)
		}
		for _, value := range entry.Standby {
			add("standby", value)
		}
		if len(items) > maxDeliveryItems {
			return nil, errors.New("meta webhook envelope has too many items")
		}
	}
	return items, nil
}
func messagingField(object string, value jsontext.Value) string {
	var event map[string]jsontext.Value
	if json.Unmarshal(value, &event) != nil {
		return "messaging_unknown"
	}
	if _, ok := event["message"]; ok {
		var message struct {
			Echo bool `json:"is_echo"`
		}
		_ = json.Unmarshal(event["message"], &message)
		if object == "page" && message.Echo {
			return "message_echoes"
		}
		return "messages"
	}
	for _, pair := range [][2]string{{"reaction", "message_reactions"}, {"message_edit", "message_edits"}, {"read", "message_reads"}, {"delivery", "message_deliveries"}, {"postback", "messaging_postbacks"}, {"referral", "messaging_referrals"}, {"optin", "messaging_optins"}, {"pass_thread_control", "messaging_handovers"}, {"take_thread_control", "messaging_handovers"}, {"request_thread_control", "messaging_handovers"}, {"app_roles", "messaging_handovers"}} {
		if _, ok := event[pair[0]]; !ok {
			continue
		}
		field := pair[1]
		if object == "instagram" {
			switch field {
			case "message_reads":
				field = "messaging_seen"
			case "messaging_referrals":
				field = "messaging_referral"
			}
		}
		return field
	}
	return "messaging_unknown"
}
