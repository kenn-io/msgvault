package whoknows

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"
)

func roleWeight(role string) float64 {
	switch role {
	case "from":
		return 1
	case "to":
		return .15
	default:
		return .05
	}
}
func newer(a, b *time.Time) int {
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	if b == nil {
		return 1
	}
	return a.Compare(*b)
}
func temperature(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

// Rank is deterministic and performs no I/O. It retains full precision.
func Rank(candidates []Candidate, p Params, now time.Time) []Ranked {
	out := make([]Ranked, 0, len(candidates))
	for _, c := range candidates {
		r := Ranked{PersonID: c.PersonID, ParticipantID: c.ParticipantID, VCardUID: c.VCardUID, DisplayName: c.DisplayName, EmailAddress: c.EmailAddress, Flags: []string{}, Evidence: []Evidence{}, Organizations: append([]Organization{}, c.Organizations...), Tiebreak: TieValue{Temperature: c.Temperature, LastContactAt: c.LastContactAt}}
		b := Breakdown{Breadth: 1, ProfileSearch: c.ProfileSearch, ProfileScore: c.ProfileScore, ChannelsAuthored: []string{}, AskMeAboutMatches: append([]string{}, c.AskMeAboutMatches...), OrganizationMatches: []string{}}
		channels := map[string]bool{}
		threads := map[int64]bool{}
		byMessage := map[int64]Evidence{}
		for _, e := range c.Evidence {
			if e.MessageType == "meeting_transcript" {
				e.Role = "conversation_member"
				if !slices.Contains(e.Flags, "meeting") {
					e.Flags = append(slices.Clone(e.Flags), "meeting")
				}
			}
			prior, ok := byMessage[e.MessageID]
			if !ok || roleWeight(e.Role) > roleWeight(prior.Role) || (roleWeight(e.Role) == roleWeight(prior.Role) && e.Role < prior.Role) {
				byMessage[e.MessageID] = e
			}
		}
		messageIDs := make([]int64, 0, len(byMessage))
		for id := range byMessage {
			messageIDs = append(messageIDs, id)
		}
		slices.Sort(messageIDs)
		for _, id := range messageIDs {
			e := byMessage[id]
			e.Flags = append([]string{}, e.Flags...)
			e.Weight = roleWeight(e.Role)
			e.Recency = .1
			if p.HalfLifeDays == 0 {
				e.Recency = 1
			} else if e.SentAt != nil {
				age := math.Max(0, now.Sub(*e.SentAt).Hours()/24)
				e.Recency = math.Max(.1, math.Exp2(-age/float64(p.HalfLifeDays)))
			}
			e.Contribution = e.Weight * e.Relevance * e.Recency
			if e.Role == "from" {
				if slices.Contains(e.Flags, "via_list") {
					e.Contribution *= .5
				}
				if c.OneWay {
					e.Contribution *= .5
				}
				b.RawAuthored += e.Contribution
				b.AuthoredMessages++
				channel := e.Channel
				if channel == "" {
					channel = "other"
				}
				channels[channel] = true
				if e.ConversationID != 0 {
					threads[e.ConversationID] = true
				}
				if newer(e.SentAt, b.LatestAuthoredAt) > 0 {
					b.LatestAuthoredAt = e.SentAt
				}
			} else {
				b.RawReceived += e.Contribution
				b.ReceivedMessages++
			}
			r.Evidence = append(r.Evidence, e)
		}
		for channel := range channels {
			b.ChannelsAuthored = append(b.ChannelsAuthored, channel)
		}
		slices.Sort(b.ChannelsAuthored)
		b.ThreadsAuthored = len(threads)
		b.Depth = math.Log1p(b.RawAuthored) + math.Min(.3, math.Log1p(b.RawReceived))
		b.Breadth = 1 + .25*float64(max(0, len(channels)-1)) + .1*float64(min(5, max(0, len(threads)-1)))
		b.Evidence = b.Depth * b.Breadth
		if len(c.AskMeAboutMatches) > 0 {
			b.Declared = .75
		}
		if c.ProfileScore != nil && *c.ProfileScore >= p.MinProfileScore {
			b.ProfileSim = 1
			if p.MinProfileScore < 1 {
				b.ProfileSim = math.Max(0, math.Min(1, (*c.ProfileScore-p.MinProfileScore)/(1-p.MinProfileScore)))
			}
		}
		for _, o := range c.Organizations {
			if organizationMatches(pTopic(p), o) {
				b.OrgMatch = 1
				b.OrganizationMatches = append(b.OrganizationMatches, o.Name)
			}
		}
		b.Affinity = .25*b.ProfileSim + .25*b.OrgMatch
		r.Score = b.Evidence + b.Declared + b.Affinity
		if c.OneWay {
			r.Flags = append(r.Flags, "one_way")
		}
		if len(c.Evidence) == 0 {
			if b.Declared > 0 {
				r.Flags = append(r.Flags, "declared_only")
			} else if b.ProfileSim > 0 {
				r.Flags = append(r.Flags, "profile_only")
			}
		}
		slices.SortFunc(r.Evidence, func(a, b Evidence) int {
			if v := cmp.Compare(b.Contribution, a.Contribution); v != 0 {
				return v
			}
			if v := newer(b.SentAt, a.SentAt); v != 0 {
				return v
			}
			return cmp.Compare(a.MessageID, b.MessageID)
		})
		b.EvidenceTotal = len(r.Evidence)
		r.Evidence = r.Evidence[:min(p.Evidence, len(r.Evidence))]
		for _, e := range r.Evidence {
			b.ShownContribution += e.Contribution
		}
		b.OmittedContribution = b.RawAuthored + b.RawReceived - b.ShownContribution
		r.Breakdown = b
		if c.PersonID == 0 {
			r.PromoteHint = fmt.Sprintf("msgvault person promote %d", c.ParticipantID)
		}
		if r.Score > 0 {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Ranked) int {
		if (a.PersonID == 0) != (b.PersonID == 0) {
			if a.PersonID == 0 {
				return 1
			}
			return -1
		}
		if a.PersonID == 0 && b.PersonID == 0 {
			if v := cmp.Compare(b.Score, a.Score); v != 0 {
				return v
			}
			return cmp.Compare(a.ParticipantID, b.ParticipantID)
		}
		if v := cmp.Compare(math.Round(b.Score*1000), math.Round(a.Score*1000)); v != 0 {
			return v
		}
		if p.Tiebreak == "temperature" {
			if v := cmp.Compare(temperature(b.Tiebreak.Temperature), temperature(a.Tiebreak.Temperature)); v != 0 {
				return v
			}
		}
		if p.Tiebreak == "last_contact" {
			if v := newer(b.Tiebreak.LastContactAt, a.Tiebreak.LastContactAt); v != 0 {
				return v
			}
		}
		if v := cmp.Compare(a.PersonID, b.PersonID); v != 0 {
			return v
		}
		return cmp.Compare(a.ParticipantID, b.ParticipantID)
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// Topic is request-local and is only used to match organization context.
func pTopic(p Params) string { return p.topic }
