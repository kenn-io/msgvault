package callhistory

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strconv"
	"time"
)

const discordJoinedParticipation = "joined"

func discordID(value string) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == value
}

func (r ImportRequest) discordObservation() (Observation, error) {
	if !discordID(r.Account) {
		return Observation{}, invalid("Discord account must be the owner's canonical user ID")
	}
	var message struct {
		ID        string         `json:"id"`
		Type      jsontext.Value `json:"type"`
		Timestamp time.Time      `json:"timestamp"`
	}
	if err := json.Unmarshal(r.Record, &message); err != nil || !discordID(message.ID) {
		return Observation{}, invalid("Discord message ID, timestamp or call fields")
	}
	prefix := "native/"
	var ended *time.Time
	var roster []string
	if r.Format == "discord" {
		var native struct {
			ChannelID string `json:"channel_id"`
			Call      *struct {
				Participants []string   `json:"participants"`
				EndedAt      *time.Time `json:"ended_timestamp"`
			} `json:"call"`
		}
		if json.Unmarshal(r.Record, &native) != nil {
			return Observation{}, invalid("Discord native call fields")
		}
		var kind int
		if json.Unmarshal(message.Type, &kind) != nil || kind != 3 || !discordID(native.ChannelID) {
			return Observation{}, invalid("Discord requires a native CALL (type 3) message and channel ID")
		}
		if native.Call != nil {
			roster, ended = native.Call.Participants, native.Call.EndedAt
		}
	} else {
		var kind string
		if json.Unmarshal(message.Type, &kind) != nil || kind != "Call" || len(r.Bindings) != 0 {
			return Observation{}, invalid("DiscordChatExporter requires one Call message; participant bindings are unavailable")
		}
		var exported struct {
			EndedAt *time.Time `json:"callEndedTimestamp"`
		}
		if json.Unmarshal(r.Record, &exported) != nil {
			return Observation{}, invalid("DiscordChatExporter end timestamp")
		}
		prefix, ended = "exporter/", exported.EndedAt
	}
	if len(roster) > 100 {
		return Observation{}, invalid("Discord participant roster exceeds 100")
	}
	o := Observation{
		RecordID: prefix + message.ID,
		Version:  1, Provider: "discord", Account: r.Account, NativeCallID: message.ID,
		Kind: "call", Direction: unknown, Outcome: unknown, OwnerParticipation: unknown,
		OccurredAt: message.Timestamp, EndedAt: ended, Precision: "instant", Raw: r.Record,
	}
	seen := make(map[string]bool, len(roster))
	for _, id := range roster {
		if !discordID(id) {
			return Observation{}, invalid("Discord participant ID")
		}
		seen[id] = true
		if id == r.Account {
			o.OwnerParticipation = discordJoinedParticipation
		}
	}
	peers := make(map[int64]bool, len(r.Bindings))
	for native, id := range r.Bindings {
		if native == r.Account || !seen[native] {
			return Observation{}, invalid("Discord binding must name a participating peer")
		}
		peers[id] = true
	}
	for id := range peers {
		o.Participants = append(o.Participants, Participant{ID: id, Participation: discordJoinedParticipation})
	}
	slices.SortFunc(o.Participants, func(a, b Participant) int { return cmp.Compare(a.ID, b.ID) })
	// Participation and an end timestamp do not establish an answered outcome
	// or talk duration. Authors and mentions are never an attendance roster.
	return o, nil
}
