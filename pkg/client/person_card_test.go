package client_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestPersonCardGeneratedNullableValuesRoundTrip(t *testing.T) {
	requirements := require.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	opts := personcard.Options{AsOf: now, Format: personcard.FormatJSON}
	inputs := personcard.Inputs{
		CollectedAt: now,
		Person:      personcard.Person{PersonID: 7},
		ContactState: &personcard.ContactState{
			LastContactAt: &at,
			Channel:       "email",
			Ref:           "message:42",
			ComputedAt:    now,
		},
		Actions:  []personcard.Action{{MessageID: 42, Ordinal: 1, Status: "pending", Text: "Follow up", OccurredAt: at, SourceID: 1}},
		Facts:    []personcard.FactChange{{ID: 5, User: true, ChangedAt: at, Label: "Office", Text: "Example City"}},
		Meetings: []personcard.Meeting{{Ref: "evt:43", Start: at, Basis: "invited"}},
	}
	inputs.Freshness.Sources = []personcard.SourceInput{{SourceID: 1, SourceType: "imap", Identifier: "owner@example.com"}}
	ok, err := personcard.BuildResult("person:7", personcard.Assemble(inputs, opts), opts)
	requirements.NoError(err)
	requirements.NotNil(ok.Card)
	requirements.Len(ok.Card.Items, 2)
	requirements.NotNil(ok.Card.LastInteraction)
	requirements.Len(ok.Card.Meetings.Recent, 1)
	inputs.Person.UntrustedText = personcard.PersonText{DisplayName: strings.Repeat("x", 120), OrganizationName: strings.Repeat("x", 120), Title: strings.Repeat("x", 120)}
	inputs.Brief = &personcard.Brief{Version: 1, GeneratedAt: now, UntrustedText: &personcard.BriefText{Text: strings.Repeat("📋", 300)}}
	opts.MaxBytes = 2048
	budgeted, err := personcard.BuildResult("person:7", personcard.Assemble(inputs, opts), opts)
	requirements.NoError(err)
	requirements.True(budgeted.Omitted.Brief)
	pendingActivityInputs := personcard.Inputs{
		CollectedAt: now,
		Person:      personcard.Person{PersonID: 7},
		ContactState: &personcard.ContactState{
			Stale: true,
		},
	}
	pendingActivity, err := personcard.BuildResult("person:7", personcard.Assemble(pendingActivityInputs, opts), opts)
	requirements.NoError(err)
	for name, result := range map[string]personcard.PersonCardResult{
		"unknown":          personcard.ResultForStatus("unknown", "email:missing@example.com", now, opts, nil, nil),
		"observed_only":    personcard.ResultForStatus("observed_only", "participant:9", now, opts, nil, new(int64(9))),
		"ambiguous":        personcard.ResultForStatus("ambiguous", "email:alex@example.com", now, opts, []personcard.Candidate{{PersonID: 7}, {PersonID: 8}}, nil),
		"pending_activity": pendingActivity,
		"ok":               ok,
		"budgeted":         budgeted,
	} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)

			wire, err := json.Marshal(result)
			requirements.NoError(err)
			var dto generated.PersonCardResult
			requirements.NoError(json.Unmarshal(wire, &dto))
			requirements.NoError(dto.Validate(), "generated validation accepts production person-card responses")
			roundTrip, err := json.Marshal(dto)
			requirements.NoError(err)
			assert.JSONEq(t, string(wire), string(roundTrip))
		})
	}
}
