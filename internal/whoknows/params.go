// Package whoknows ranks people by cited, archived topical evidence.
package whoknows

import (
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var ErrMissingFreeText = errors.New("topic must contain free text")
var ErrInvalidParameter = errors.New("invalid parameter")

const MaxCandidatePeople = 500
const MaxDeclaredValues = 20000

var AutomatedPatterns = []string{"no-reply", "noreply", "do-not-reply", "donotreply", "notification", "notifications", "notify", "alert", "alerts", "newsletter", "newsletters", "news", "digest", "mailer-daemon", "postmaster", "bounce", "bounces", "updates", "automated", "robot", "bot", "calendar-notification", "jira", "github", "gitlab", "linear", "notion", "slack", "zoom", "calendar"}

type WhoKnowsParams struct {
	topic                string
	Limit                int                `json:"limit"`
	Evidence             int                `json:"evidence"`
	Candidates           int                `json:"candidates"`
	HalfLifeDays         int                `json:"half_life_days"`
	MinScore             float64            `json:"min_score"`
	MinProfileScore      float64            `json:"min_profile_score"`
	IncludeOwner         bool               `json:"include_owner"`
	IncludeUnsaved       bool               `json:"include_unsaved"`
	IncludeOrganizations bool               `json:"include_organizations"`
	Tiebreak             string             `json:"tiebreak"`
	After                *time.Time         `json:"after"`
	Before               *time.Time         `json:"before"`
	Excerpts             bool               `json:"-"`
	Weights              map[string]float64 `json:"weights"`
	AutomatedPatterns    []string           `json:"automated_patterns"`
	ExcludedMessageTypes []string           `json:"excluded_message_types"`
	MaxRosterEdges       int                `json:"max_roster_edges"`
	MaxCandidatePeople   int                `json:"max_candidate_people"`
}

func DefaultParams() Params {
	return Params{Limit: 10, Evidence: 3, Candidates: 200, HalfLifeDays: 180, MinProfileScore: .5, IncludeUnsaved: true, IncludeOrganizations: true, Tiebreak: "temperature", Weights: map[string]float64{"from": 1, "to": .15, "cc": .05, "bcc": .05, "conversation_member": .05, "received_cap": .3, "declared": .75, "profile_sim": .25, "org_match": .25, "recency_floor": .1, "breadth_channel_step": .25, "breadth_thread_step": .1, "breadth_thread_cap": 5, "list_factor": .5, "one_way_factor": .5}, AutomatedPatterns: append([]string{}, AutomatedPatterns...), ExcludedMessageTypes: []string{"calendar_event"}, MaxRosterEdges: store.MaxRosterEdges, MaxCandidatePeople: MaxCandidatePeople}
}
func (p Params) Validate() error {
	for _, v := range []struct {
		name        string
		v, min, max int
	}{{"limit", p.Limit, 1, 50}, {"evidence", p.Evidence, 0, 10}, {"candidates", p.Candidates, 50, 1000}, {"half_life_days", p.HalfLifeDays, 0, 3650}} {
		if v.v < v.min || v.v > v.max {
			return fmt.Errorf("%s must be between %d and %d", v.name, v.min, v.max)
		}
	}
	if math.IsNaN(p.MinScore) || math.IsInf(p.MinScore, 0) {
		return errors.New("min_score must be finite")
	}
	if math.IsNaN(p.MinProfileScore) || p.MinProfileScore < 0 || p.MinProfileScore > 1 {
		return errors.New("min_profile_score must be between 0 and 1")
	}
	if p.Tiebreak != "temperature" && p.Tiebreak != "last_contact" && p.Tiebreak != "none" {
		return errors.New("tiebreak must be temperature, last_contact, or none")
	}
	if p.After != nil && p.Before != nil && !p.After.Before(*p.Before) {
		return errors.New("after must precede before")
	}
	if p.MaxRosterEdges < 1 || p.MaxRosterEdges > store.MaxRosterEdges {
		return fmt.Errorf("max_roster_edges must be between 1 and %d", store.MaxRosterEdges)
	}
	if p.MaxCandidatePeople < 1 || p.MaxCandidatePeople > MaxCandidatePeople {
		return fmt.Errorf("max_candidate_people must be between 1 and %d", MaxCandidatePeople)
	}
	return nil
}

// ParseDate preserves the inclusive named-day CLI/API convention for before.
func ParseDate(raw string, before bool) (*time.Time, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // An omitted date leaves the filter unbounded.
	}
	layout := time.RFC3339
	if len(raw) == 10 {
		layout = time.DateOnly
	}
	v, err := time.Parse(layout, raw)
	if err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD or RFC3339: %w", err)
	}
	if before && layout == time.DateOnly {
		v = v.AddDate(0, 0, 1)
	}
	return &v, nil
}

type Params = WhoKnowsParams
