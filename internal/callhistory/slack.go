package callhistory

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"time"
)

// SlackHuddlePeerIDs validates a huddle and returns associated native user IDs,
// excluding the source owner. Room rosters do not establish joined attendance.
func SlackHuddlePeerIDs(account string, raw jsontext.Value) ([]string, error) {
	_, peers, err := parseSlackHuddle(account, raw)
	return peers, err
}

func parseSlackHuddle(account string, raw jsontext.Value) (Observation, []string, error) {
	team, owner, ok := strings.Cut(account, ":")
	if !ok || !validID(account) || !validID(team) || !validID(owner) || strings.Contains(owner, ":") || len(raw) > MaxRawBytes {
		return Observation{}, nil, invalid("Slack account requires workspace:owner")
	}
	var message struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Team    string `json:"team"`
		TS      string `json:"ts"`
		Room    struct {
			ID           string   `json:"id"`
			Start        int64    `json:"date_start"`
			End          int64    `json:"date_end"`
			History      []string `json:"participant_history"`
			Participants []string `json:"participants"`
		} `json:"room"`
	}
	if err := json.Unmarshal(raw, &message); err != nil || message.Type != "message" || message.Subtype != "huddle_thread" || !validID(message.TS) || (message.Team != "" && message.Team != team) {
		return Observation{}, nil, invalid("Slack huddle message")
	}
	room := message.Room
	// Guard before time.Unix: extreme int64 values must not wrap into a valid year.
	const maxUnixSeconds int64 = 253402300799
	if room.Start <= 0 || room.Start > maxUnixSeconds || room.End < 0 || room.End > maxUnixSeconds || (room.End != 0 && room.End < room.Start) {
		return Observation{}, nil, invalid("Slack room timestamps")
	}
	evidence := Evidence{Version: 1, Provider: "slack", Account: account, NativeCallID: room.ID,
		Kind: "call", Direction: unknown, Outcome: unknown, OwnerParticipation: unknown,
		OccurredAt: time.Unix(room.Start, 0).UTC(), Precision: "instant"}
	o := Observation{
		RecordID: room.ID,
		Evidence: evidence,
		Raw:      raw,
	}
	if room.End > 0 {
		end := time.Unix(room.End, 0).UTC()
		o.EndedAt = &end
	}
	if !validID(room.ID) {
		return Observation{}, nil, invalid("Slack room identity")
	}
	if err := o.Validate(); err != nil {
		return Observation{}, nil, err
	}
	seen := map[string]bool{}
	for _, roster := range [][]string{room.History, room.Participants} {
		for _, id := range roster {
			if !validID(id) {
				return Observation{}, nil, invalid("Slack room participant")
			}
			if id != owner {
				seen[id] = true
			}
		}
	}
	peers := make([]string, 0, len(seen))
	for id := range seen {
		peers = append(peers, id)
	}
	slices.Sort(peers)
	return o, peers, nil
}

func (r ImportRequest) slackObservation() (Observation, error) {
	o, peers, err := parseSlackHuddle(r.Account, r.Record)
	if err != nil {
		return Observation{}, err
	}
	used := 0
	ids := map[int64]bool{}
	for _, peer := range peers {
		if id, ok := r.Bindings[peer]; ok {
			ids[id] = true
			used++
		}
	}
	if used != len(r.Bindings) {
		return Observation{}, invalid("Slack binding must name an associated peer")
	}
	for id := range ids {
		o.Participants = append(o.Participants, Participant{ID: id, Participation: unknown})
	}
	slices.SortFunc(o.Participants, func(a, b Participant) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return o, nil
}
