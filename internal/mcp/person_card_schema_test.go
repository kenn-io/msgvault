package mcp

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personcard"
)

// Google schema inference must describe the actual wire values, including
// null optional projections and the brief body removed by byte budgeting.
func TestPersonCardOutputSchemaAcceptsEncodedResults(t *testing.T) {
	requirements := require.New(t)

	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	opts := personcard.Options{AsOf: now, Format: personcard.FormatJSON, MaxBytes: 65536}
	inputs := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: 7, UntrustedText: personcard.PersonText{DisplayName: "Alex Example"}}}
	card := personcard.Assemble(inputs, opts)
	ok, err := personcard.BuildResult("person:7", card, opts)
	requirements.NoError(err)
	requirements.NotNil(ok.Card)
	requirements.Nil(ok.Card.Brief)
	requirements.Nil(ok.Card.LastInteraction)
	requirements.Nil(ok.Freshness.Projections["open_loops"])

	inputs.Person.UntrustedText = personcard.PersonText{DisplayName: strings.Repeat("x", 120), OrganizationName: strings.Repeat("x", 120), Title: strings.Repeat("x", 120)}
	inputs.Brief = &personcard.Brief{Version: 1, GeneratedAt: now, UntrustedText: &personcard.BriefText{Text: strings.Repeat("📋", 300)}}
	budgetOpts := opts
	budgetOpts.MaxBytes = 2048
	budgeted, err := personcard.BuildResult("person:7", personcard.Assemble(inputs, budgetOpts), budgetOpts)
	requirements.NoError(err)
	requirements.NotNil(budgeted.Card)
	requirements.NotNil(budgeted.Card.Brief)
	requirements.Nil(budgeted.Card.Brief.UntrustedText)
	requirements.True(budgeted.Omitted.Brief)

	results := map[string]personcard.PersonCardResult{
		"unknown":            personcard.ResultForStatus("unknown", "email:missing@example.com", now, opts, nil, nil),
		"observed_only":      personcard.ResultForStatus("observed_only", "participant:9", now, opts, nil, new(int64(9))),
		"ambiguous":          personcard.ResultForStatus("ambiguous", "email:alex@example.com", now, opts, []personcard.Candidate{{PersonID: 7}, {PersonID: 8}}, nil),
		"ok nullable fields": ok,
		"budgeted brief":     budgeted,
	}
	schema := outputSchemaFor[personcard.PersonCardResult]()
	resolved, err := schema.Resolve(nil)
	requirements.NoError(err)
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(result, json.Deterministic(true))
			require.NoError(t, err)
			var structured any
			require.NoError(t, json.Unmarshal(encoded, &structured))
			require.NoError(t, resolved.Validate(structured))
		})
	}
}
