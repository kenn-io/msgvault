package callhistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Teams events describe call activity, not an owner-relative disposition.
// Only an ended event supplies a participant roster; an initiator is not one.
func (r ImportRequest) teamsObservation() (Observation, error) {
	var message struct {
		ID              string     `json:"id"`
		ChatID          string     `json:"chatId"`
		ReplyToID       string     `json:"replyToId"`
		MessageType     string     `json:"messageType"`
		CreatedDateTime *time.Time `json:"createdDateTime"`
		DeletedDateTime *time.Time `json:"deletedDateTime"`
		ChannelIdentity *struct {
			TeamID    string `json:"teamId"`
			ChannelID string `json:"channelId"`
		} `json:"channelIdentity"`
		EventDetail struct {
			Type             string         `json:"@odata.type"`
			CallID           string         `json:"callId"`
			CallEventType    string         `json:"callEventType"`
			CallDuration     jsontext.Value `json:"callDuration"`
			CallParticipants jsontext.Value `json:"callParticipants"`
		} `json:"eventDetail"`
	}
	if err := json.Unmarshal(r.Record, &message); err != nil {
		return Observation{}, invalid("Teams message JSON")
	}
	detail := message.EventDetail
	started := detail.Type == "#microsoft.graph.callStartedEventMessageDetail"
	ended := detail.Type == "#microsoft.graph.callEndedEventMessageDetail"
	if message.MessageType != "systemEventMessage" || (!started && !ended) ||
		!validID(message.ID) || !validID(detail.CallID) || strings.TrimSpace(detail.CallEventType) == "" ||
		message.CreatedDateTime == nil || (message.ReplyToID != "" && !validID(message.ReplyToID)) {
		return Observation{}, invalid("Teams call event")
	}
	context := []string{"chat", message.ChatID}
	if message.ChannelIdentity != nil {
		channel := message.ChannelIdentity
		if message.ChatID != "" || !validID(channel.TeamID) || !validID(channel.ChannelID) {
			return Observation{}, invalid("Teams channel context")
		}
		context = []string{"channel", channel.TeamID, channel.ChannelID}
	} else if !validID(message.ChatID) {
		return Observation{}, invalid("Teams chat context")
	}
	// JSON array encoding separates arbitrary delimiters and context kinds.
	context = append(context, message.ReplyToID, message.ID)
	key, err := json.Marshal(context)
	if err != nil {
		return Observation{}, invalid("Teams message identity")
	}
	hash := sha256.Sum256(key)
	o := Observation{
		RecordID: hex.EncodeToString(hash[:]),
		Version:  1, Provider: "teams", Account: r.Account, NativeCallID: detail.CallID,
		Kind: "call", Direction: unknown, Outcome: unknown, OwnerParticipation: unknown,
		OccurredAt: message.CreatedDateTime.UTC(), Precision: "instant",
		Withdrawn: message.DeletedDateTime != nil, Raw: r.Record,
	}
	if started {
		if len(r.Bindings) != 0 {
			return Observation{}, invalid("Teams started events have no attended roster")
		}
		return o, nil
	}
	end := o.OccurredAt
	o.EndedAt = &end
	if len(detail.CallDuration) != 0 && string(detail.CallDuration) != "null" {
		var text string
		if err := json.Unmarshal(detail.CallDuration, &text); err != nil {
			return Observation{}, invalid("Teams call duration")
		}
		duration, err := teamsDuration(text)
		if err != nil {
			return Observation{}, err
		}
		seconds := duration.Seconds()
		o.DurationSeconds = &seconds
		o.OccurredAt = end.Add(-duration)
	}
	var roster []struct {
		Participant struct {
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"participant"`
	}
	if len(detail.CallParticipants) != 0 {
		if err := json.Unmarshal(detail.CallParticipants, &roster); err != nil {
			return Observation{}, invalid("Teams call participants")
		}
	}
	native := make(map[string]bool, len(roster))
	for _, entry := range roster {
		if entry.Participant.User != nil {
			native[entry.Participant.User.ID] = true
		}
	}
	ids := make(map[int64]bool, len(r.Bindings))
	for id, participant := range r.Bindings {
		if !native[id] {
			return Observation{}, invalid("Teams binding is not in the ended call roster")
		}
		ids[participant] = true
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	for _, id := range ordered {
		o.Participants = append(o.Participants, Participant{ID: id, Participation: "joined"})
	}
	return o, nil
}

var teamsDurationPattern = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// Graph uses ISO 8601 durations. Calendar units are deliberately unsupported;
// fixed durations must fit time.Duration, with nanosecond precision.
func teamsDuration(text string) (time.Duration, error) {
	parts := teamsDurationPattern.FindStringSubmatch(text)
	if parts == nil || text == "P" || text == "PT" || strings.HasSuffix(text, "T") {
		return 0, invalid("Teams call duration")
	}
	var days time.Duration
	if parts[1] != "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n > math.MaxInt64/int64(24*time.Hour) {
			return 0, invalid("Teams call duration overflow")
		}
		days = time.Duration(n) * 24 * time.Hour
	}
	var clock strings.Builder
	for i, unit := range []string{"h", "m", "s"} {
		if parts[i+2] != "" {
			clock.WriteString(parts[i+2])
			clock.WriteString(unit)
		}
	}
	var duration time.Duration
	if clock.Len() > 0 {
		var err error
		duration, err = time.ParseDuration(clock.String())
		if err != nil {
			return 0, invalid("Teams call duration overflow")
		}
	}
	if duration > time.Duration(math.MaxInt64)-days {
		return 0, invalid("Teams call duration overflow")
	}
	return days + duration, nil
}
