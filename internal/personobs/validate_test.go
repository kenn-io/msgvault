package personobs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func validProposal() ProposedObservation {
	return ProposedObservation{PersonID: 1, Level: LevelExplicit, Origin: OriginExtraction, Kind: KindState, Content: "[PERSON] runs", Attribution: AttributionSelf, ConfidenceBP: 800, Support: []SupportInput{{Evidence: personfacts.EvidenceInput{PersonID: 1, Excerpt: "I run", Directness: personfacts.DirectSelf}, Quote: "run", QuoteStart: 2}}}
}
func TestValidateProposal(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := now.Add(-time.Hour)
	premises := map[int64]Observation{1: {ID: 1, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive}, 2: {ID: 2, PersonID: 1, Level: LevelDeductive, Status: StatusActive}, 3: {ID: 3, PersonID: 2, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive}, 4: {ID: 4, PersonID: 1, Level: LevelExplicit, Status: StatusUnsupported}, 5: {ID: 5, PersonID: 1, Level: LevelExplicit, Status: StatusRejected}, 6: {ID: 6, PersonID: 1, Level: LevelInductive, Status: StatusActive}, 7: {ID: 7, PersonID: 1, Level: LevelContradiction, Status: StatusActive}}
	derived := func(p *ProposedObservation) {
		p.Level = LevelDeductive
		p.Origin = OriginReasoning
		p.Support = nil
		p.Attribution = ""
		p.Premises = []PremiseInput{{1, PremiseRoleSupport}}
	}
	tests := []struct {
		name, reason string
		change       func(*ProposedObservation)
	}{
		{"explicit", "", func(p *ProposedObservation) {}},
		{"500 runes", "", func(p *ProposedObservation) { p.Content = "[PERSON]" + strings.Repeat("界", 492) }},
		{"unknown origin", "origin", func(p *ProposedObservation) { p.Origin = "invented" }},
		{"unknown kind", "kind", func(p *ProposedObservation) { p.Kind = "invented" }},
		{"long subject", "subject-length", func(p *ProposedObservation) { p.SubjectRef = strings.Repeat("界", 65) }},
		{"unknown stability", "stability", func(p *ProposedObservation) { p.Stability = "invented" }},
		{"missing premise", "premise-missing", func(p *ProposedObservation) { derived(p); p.Premises[0].ID = 999 }},
		{"missing attribution", "attribution", func(p *ProposedObservation) { p.Attribution = "" }},
		{"owner derived", "origin-level", func(p *ProposedObservation) { derived(p); p.Origin = OriginOwner }},
		{"reasoning explicit", "origin-level", func(p *ProposedObservation) { p.Origin = OriginReasoning }},
		{"extraction derived", "origin-level", func(p *ProposedObservation) { derived(p); p.Origin = OriginExtraction }},
		{"canonical tokens", "", func(p *ProposedObservation) { p.Content = "[person] runs" }},
		{"owner", "", func(p *ProposedObservation) { p.Origin = OriginOwner; p.Support = nil; p.Attribution = "" }},
		{"support required", "explicit-requires-support", func(p *ProposedObservation) { p.Support = nil }},
		{"context unavailable", "context-citations-unavailable", func(p *ProposedObservation) { p.ContextCount = 1 }},
		{"assent unavailable", "context-citations-unavailable", func(p *ProposedObservation) { p.Attribution = AttributionAssent }},
		{"reported unavailable", "context-citations-unavailable", func(p *ProposedObservation) { p.Attribution = AttributionReported; p.Support = nil }},
		{"explicit premises", "explicit-has-premises", func(p *ProposedObservation) { p.Premises = []PremiseInput{{1, PremiseRoleSupport}} }},
		{"deductive", "", derived},
		{"deductive no premise", "premise-count", func(p *ProposedObservation) { derived(p); p.Premises = nil }},
		{"inductive one", "premise-count", func(p *ProposedObservation) { derived(p); p.Level = LevelInductive; p.Kind = KindPattern }},
		{"inductive", "", func(p *ProposedObservation) {
			derived(p)
			p.Level = LevelInductive
			p.Kind = KindPattern
			p.Premises = append(p.Premises, PremiseInput{2, PremiseRoleSupport})
		}},
		{"contradiction one", "premise-count", func(p *ProposedObservation) { derived(p); p.Level = LevelContradiction }},
		{"contradiction", "", func(p *ProposedObservation) {
			derived(p)
			p.Level = LevelContradiction
			p.Premises = append(p.Premises, PremiseInput{6, PremiseRoleCounter})
		}},
		{"cross person", "premise-person", func(p *ProposedObservation) { derived(p); p.Premises[0].ID = 3 }},
		{"unsupported", "premise-not-supported", func(p *ProposedObservation) { derived(p); p.Premises[0].ID = 4 }},
		{"counter not live", "premise-not-live", func(p *ProposedObservation) {
			derived(p)
			p.Level = LevelContradiction
			p.Premises = append(p.Premises, PremiseInput{5, PremiseRoleCounter})
		}},
		{"inductive premise", "premise-level", func(p *ProposedObservation) { derived(p); p.Premises[0].ID = 6 }},
		{"contradiction premise", "premise-level", func(p *ProposedObservation) { derived(p); p.Premises[0].ID = 7 }},
		{"token", "missing-person-token", func(p *ProposedObservation) { p.Content = "Someone runs" }},
		{"long", "content-length", func(p *ProposedObservation) { p.Content = "[PERSON]" + strings.Repeat("界", 493) }},
		{"empty", "content-length", func(p *ProposedObservation) { p.Content = "" }},
		{"validity", "validity-order", func(p *ProposedObservation) { p.ValidFrom = &now; p.ValidUntil = &before }},
		{"quote range", "quote-out-of-range", func(p *ProposedObservation) { p.Support[0].QuoteStart = 20 }},
		{"quote mismatch", "quote-out-of-range", func(p *ProposedObservation) { p.Support[0].Quote = "ran" }},
		{"negative offset", "quote-out-of-range", func(p *ProposedObservation) { p.Support[0].QuoteStart = -1 }},
		{"confidence", "confidence-range", func(p *ProposedObservation) { p.ConfidenceBP = 1001 }},
		{"unknown level", "level", func(p *ProposedObservation) { p.Level = "invented" }},
		{"unknown category", "category", func(p *ProposedObservation) { p.ModelCategories = []Category{"invented"} }},
		{"half target", "target-pair", func(p *ProposedObservation) { p.TargetKey = "x" }},
		{"derived evidence", "derived-has-support", func(p *ProposedObservation) { derived(p); p.Support = validProposal().Support }},
		{"duplicate premises", "premise-duplicate", func(p *ProposedObservation) { derived(p); p.Premises = append(p.Premises, p.Premises[0]) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProposal()
			tt.change(&p)
			err := ValidateProposal(p, premises)
			if tt.reason == "" {
				require.Nil(t, err)
			} else {
				require.NotNil(t, err)
				assert.Equal(t, tt.reason, err.Reason)
			}
		})
	}
}
func TestValidateAppend(t *testing.T) {
	tests := []struct {
		name             string
		derived, premise Observation
		role             PremiseRole
		reason           string
	}{
		{"deductive order", Observation{ID: 10, PersonID: 1, Level: LevelDeductive}, Observation{ID: 12, PersonID: 1, Level: LevelDeductive, Status: StatusActive}, PremiseRoleSupport, "premise-order"},
		{"new explicit", Observation{ID: 10, PersonID: 1, Level: LevelInductive}, Observation{ID: 12, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive}, PremiseRoleSupport, ""},
		{"cross person", Observation{ID: 10, PersonID: 1, Level: LevelInductive}, Observation{ID: 12, PersonID: 2, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive}, PremiseRoleSupport, "premise-person"},
		{"self edge", Observation{ID: 10, PersonID: 1, Level: LevelDeductive}, Observation{ID: 10, PersonID: 1, Level: LevelDeductive, Status: StatusActive}, PremiseRoleSupport, "premise-order"},
		{"unsupported", Observation{ID: 10, PersonID: 1, Level: LevelDeductive}, Observation{ID: 2, PersonID: 1, Level: LevelExplicit, Status: StatusUnsupported}, PremiseRoleSupport, "premise-not-supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ValidateAppend(tt.derived, tt.premise, tt.role)
			if tt.reason == "" {
				require.Nil(t, e)
			} else {
				require.NotNil(t, e)
				assert.Equal(t, tt.reason, e.Reason)
			}
		})
	}
}

func TestValidateProposalInductiveRequiresPattern(t *testing.T) {
	p := validProposal()
	p.Level, p.Origin, p.Kind = LevelInductive, OriginReasoning, KindState
	p.Support = nil
	p.Premises = []PremiseInput{{1, PremiseRoleSupport}, {2, PremiseRoleSupport}}
	premises := map[int64]Observation{
		1: {ID: 1, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive},
		2: {ID: 2, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive},
	}
	failure := ValidateProposal(p, premises)
	require.NotNil(t, failure)
	assert.Equal(t, "inductive-kind", failure.Reason)
}

func TestValidateProposalContradictionCountsSupportedPremises(t *testing.T) {
	for _, state := range []struct {
		name   string
		status Status
		flag   SupportFlag
	}{
		{"withdrawn", StatusUnsupported, SupportFlagNone},
		{"flagged", StatusActive, SupportFlagLapsed},
	} {
		t.Run(state.name, func(t *testing.T) {
			p := validProposal()
			p.Level, p.Origin = LevelContradiction, OriginReasoning
			p.Support = nil
			p.Premises = []PremiseInput{{1, PremiseRoleSupport}, {2, PremiseRoleCounter}}
			premises := map[int64]Observation{
				1: {ID: 1, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive},
				2: {ID: 2, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: state.status, SupportFlag: state.flag},
			}
			failure := ValidateProposal(p, premises)
			require.NotNil(t, failure)
			assert.Equal(t, "premise-count", failure.Reason)
			// An additional supported counter supplies the second supported premise.
			premises[3] = Observation{ID: 3, PersonID: 1, Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive}
			p.Premises = append(p.Premises, PremiseInput{3, PremiseRoleCounter})
			require.Nil(t, ValidateProposal(p, premises))
		})
	}
}

func TestValidateProposalOccursOn(t *testing.T) {
	for _, tt := range []struct {
		name  string
		date  *personfacts.PartialDateValue
		valid bool
	}{
		{"absent", nil, true}, {"year", &personfacts.PartialDateValue{Year: 2026}, true},
		{"month", &personfacts.PartialDateValue{Year: 2026, Month: 10}, true},
		{"leap day", &personfacts.PartialDateValue{Year: 2024, Month: 2, Day: 29}, true},
		{"lower year", &personfacts.PartialDateValue{Year: 1}, true},
		{"upper date", &personfacts.PartialDateValue{Year: 9999, Month: 12, Day: 31}, true},
		{"zero year", &personfacts.PartialDateValue{}, false},
		{"negative year", &personfacts.PartialDateValue{Year: -1}, false},
		{"overflow year", &personfacts.PartialDateValue{Year: 10000}, false},
		{"month 13", &personfacts.PartialDateValue{Year: 2026, Month: 13}, false},
		{"negative month", &personfacts.PartialDateValue{Year: 2026, Month: -1}, false},
		{"day without month", &personfacts.PartialDateValue{Year: 2026, Day: 1}, false},
		{"negative day", &personfacts.PartialDateValue{Year: 2026, Month: 1, Day: -1}, false},
		{"day 32", &personfacts.PartialDateValue{Year: 2026, Month: 1, Day: 32}, false},
		{"non leap day", &personfacts.PartialDateValue{Year: 2026, Month: 2, Day: 29}, false},
		{"April 31", &personfacts.PartialDateValue{Year: 2026, Month: 4, Day: 31}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := validProposal()
			p.OccursOn = tt.date
			failure := ValidateProposal(p, nil)
			if tt.valid {
				require.Nil(t, failure)
			} else {
				require.NotNil(t, failure)
				assert.Equal(t, "occurs-on", failure.Reason)
			}
		})
	}
}
func TestValidateProposalRejectsControls(t *testing.T) {
	for control := range 32 {
		for _, field := range []string{"content", "subject_ref", "quote", "excerpt"} {
			t.Run(fmt.Sprintf("%s-%02x", field, control), func(t *testing.T) {
				p := validProposal()
				value := "runs" + string(rune(control))
				switch field {
				case "content":
					p.Content = "[PERSON] " + value
				case "subject_ref":
					p.SubjectRef = value
				case "quote":
					p.Support[0].Quote = value
					p.Support[0].Evidence.Excerpt = value
					p.Support[0].QuoteStart = 0
				case "excerpt":
					p.Support[0].Evidence.Excerpt += value
				}
				failure := ValidateProposal(p, nil)
				if control == '\t' || control == '\n' {
					require.Nil(t, failure)
				} else {
					require.NotNil(t, failure)
					assert.Contains(t, failure.Reason, "control-character")
				}
			})
		}
	}
}
