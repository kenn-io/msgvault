package callhistory

import (
	"encoding/json/v2"
	"time"
)

func (r ImportRequest) signalObservation() (Observation, error) {
	var row struct {
		CallID         string `json:"callId"`
		PeerID         string `json:"peerId"`
		Mode           string `json:"mode"`
		Status         string `json:"status"`
		Direction      string `json:"direction"`
		Timestamp      *int64 `json:"timestamp"`
		EndedTimestamp *int64 `json:"endedTimestamp"`
	}
	if err := json.Unmarshal(r.Record, &row); err != nil || row.Timestamp == nil {
		return Observation{}, invalid("Signal row or timestamp")
	}
	o := Observation{
		RecordID: row.CallID,

		Version: 1, Provider: "signal", Account: r.Account, NativeCallID: row.CallID,
		Kind: "call", Direction: unknown, Outcome: unknown, OwnerParticipation: unknown,
		OccurredAt: time.UnixMilli(*row.Timestamp).UTC(), Precision: "instant",

		Raw: r.Record,
	}
	if row.EndedTimestamp != nil {
		end := time.UnixMilli(*row.EndedTimestamp).UTC()
		o.EndedAt = &end
	}
	switch row.Direction {
	case "Incoming":
		o.Direction = "incoming"
	case "Outgoing":
		o.Direction = "outgoing"
	}
	knownMode := row.Mode == "Direct" || row.Mode == "Group" || row.Mode == "Adhoc"
	if knownMode {
		switch row.Status {
		case "Deleted":
			o.Withdrawn = true
		case "Missed", "MissedNotificationProfile":
			if row.Mode != "Adhoc" {
				o.Outcome, o.OwnerParticipation = "missed", "not_joined"
			}
		case "Declined":
			if row.Mode != "Adhoc" {
				o.Outcome, o.OwnerParticipation = "declined", "not_joined"
			}
		case "Accepted":
			if row.Mode == "Direct" {
				o.Outcome, o.OwnerParticipation = "answered", "joined"
			}
		case "Joined":
			if row.Mode == "Group" {
				o.Outcome, o.OwnerParticipation = "answered", "joined"
			}
		case "JoinedAdhoc":
			if row.Mode == "Adhoc" {
				o.Outcome, o.OwnerParticipation = "answered", "joined"
			}
		}
	}
	if len(r.Bindings) != 0 {
		id, ok := r.Bindings[row.PeerID]
		if row.Mode != "Direct" || len(r.Bindings) != 1 || !ok {
			return Observation{}, invalid("Signal bindings require the exact direct peer ID")
		}
		role := unknown
		if o.Outcome == "answered" {
			role = "joined"
		}
		o.Participants = []Participant{{ID: id, Participation: role}}
	}
	// The native row contains no joined group roster or talk duration.
	return o, nil
}
