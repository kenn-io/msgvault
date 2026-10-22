package zoomcalls

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"time"

	"go.kenn.io/msgvault/internal/callhistory"
)

type optionalTime struct{ value *time.Time }

func (v *optionalTime) UnmarshalJSON(data []byte) error {
	var text *string
	if err := json.Unmarshal(data, &text); err != nil {
		return errors.New("invalid Zoom optional time")
	}
	if text == nil || *text == "" {
		v.value = nil
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, *text)
	if err != nil {
		return errors.New("invalid Zoom optional time")
	}
	v.value = &at
	return nil
}

type phoneElement struct {
	ElementID    string       `json:"call_element_id"`
	CallID       string       `json:"call_id"`
	Direction    string       `json:"direction"`
	Event        string       `json:"event"`
	Result       string       `json:"result"`
	Reason       string       `json:"result_reason"`
	Caller       string       `json:"caller_ext_id"`
	Callee       string       `json:"callee_ext_id"`
	CallerNumber string       `json:"caller_did_number"`
	CalleeNumber string       `json:"callee_did_number"`
	Handler      string       `json:"handler_ext_id"`
	Start        time.Time    `json:"start_time"`
	Answer       optionalTime `json:"answer_time"`
	End          optionalTime `json:"end_time"`
	Talk         *int64       `json:"talk_time,omitempty"`
}

// PhoneObservation preserves one source element; only an owner-attributed
// direct connected leg can confirm owner and peer attendance.
func PhoneObservation(account, owner string, raw []byte, bindings map[string]int64) (callhistory.Observation, error) {
	var row phoneElement
	var o callhistory.Observation
	if err := json.Unmarshal(raw, &row); err != nil || len(raw) > MaxRosterBytes || (!validNativeID(account) || !validNativeID(owner)) || row.ElementID == "" || row.CallID == "" || len(bindings) > 100 {
		return o, errors.New("invalid Zoom Phone element, owner or bindings")
	}
	o = callhistory.Observation{
		Version: 1, Provider: "zoom_phone", Account: AccountNamespace(account, owner), NativeCallID: row.CallID, Kind: "call",
		Outcome: "unknown", OwnerParticipation: "unknown", Direction: "unknown", Precision: "instant",
		OccurredAt: row.Start, EndedAt: row.End.value, RecordID: row.ElementID, Raw: jsontext.Value(raw)}
	peer, number, owned := "", "", false
	switch row.Direction {
	case "inbound":
		o.Direction = "incoming"
		peer, number = row.Caller, row.CallerNumber
		owned = row.Callee == owner && row.Event == "incoming"
	case "outbound":
		o.Direction = "outgoing"
		peer, number = row.Callee, row.CalleeNumber
		owned = row.Caller == owner && row.Event == "outgoing"
	}
	if row.Talk != nil && *row.Talk < 0 {
		return o, errors.New("negative Zoom Phone talk time")
	}
	connected := owned && peer != owner && row.Reason == "" && (row.Handler == "" || row.Handler == owner) &&
		(row.Result == "answered" || row.Result == "connected") && row.Answer.value != nil && !row.Answer.value.Before(row.Start) && row.End.value != nil && row.Answer.value.Before(*row.End.value)
	participation := "unknown"
	if connected {
		o.Outcome, o.OwnerParticipation, participation = "answered", "joined", "joined"
		if row.Talk != nil {
			duration := float64(*row.Talk)
			o.DurationSeconds = &duration
		}
	}
	key := "ext/" + peer
	if peer == "" {
		key = "phone/" + number
	}
	for key, id := range bindings {
		if key == "" || id <= 0 {
			return o, errors.New("invalid Zoom Phone binding")
		}
	}
	if id, ok := bindings[key]; ok && (peer != "" || number != "") && peer != owner {
		o.Participants = []callhistory.Participant{{ID: id, Participation: participation}}
	}
	if err := o.Validate(); err != nil {
		return o, fmt.Errorf("zoom Phone: %w", err)
	}
	return o, nil
}

// Phone stages the user's modern call_elements, not deprecated call_logs.
func (c *Client) Phone(ctx context.Context, from, to string) ([]jsontext.Value, error) {
	windows, err := dateWindows(from, to, time.Now())
	if err != nil {
		return nil, err
	}
	var out []jsontext.Value
	retained := 0
	for _, window := range windows {
		q := url.Values{"from": {window[0].Format(time.DateOnly)}, "to": {window[1].AddDate(0, 0, -1).Format(time.DateOnly)}, "page_size": {"100"}}
		pages, err := c.pages(ctx, "/phone/users/me/call_history", "call_elements", q, 1000)
		if err != nil {
			return nil, err
		}
		for _, raw := range pages {
			var page struct {
				Items []jsontext.Value `json:"call_elements"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return nil, errors.New("invalid Zoom Phone response")
			}
			for _, item := range page.Items {
				var row phoneElement
				if err := json.Unmarshal(item, &row); err != nil || row.Start.IsZero() {
					return nil, errors.New("invalid Zoom Phone source time")
				}
				if !row.Start.Before(window[0]) && row.Start.Before(window[1]) {
					retained += len(item)
					if retained > 32<<20 {
						return nil, errors.New("zoom staging exceeds 32 MiB")
					}
					out = append(out, item)
				}
				if len(out) > 1000 {
					return nil, errors.New("zoom Phone run exceeds 1000 elements")
				}
			}
		}
	}
	return out, nil
}
