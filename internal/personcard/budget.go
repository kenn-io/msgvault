package personcard

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"
)

var ErrBudgetTooSmall = errors.New("budget_too_small")

// Budget removes complete optional fields without mutating the input. JSON
// measures the card only; source metadata lives in the envelope, so dropping
// sources can only make Markdown smaller, never change JSON's measured bytes.
func Budget(card Card, format Format, maxBytes int) (Card, Omitted) {
	c := card
	c.Items = slices.Clone(card.Items)
	c.Meetings.Recent = slices.Clone(card.Meetings.Recent)
	c.Freshness.Sources = slices.Clone(card.Freshness.Sources)
	if c.Brief != nil {
		b := *c.Brief
		c.Brief = &b
	}
	omitted := Omitted{}
	fits := func() bool {
		size, err := PayloadBytes(c, format, c.affiliation)
		return err == nil && size <= maxBytes
	}
	if fits() {
		return c, omitted
	}
	if c.Brief != nil && c.Brief.UntrustedText != nil {
		c.Brief.UntrustedText = nil
		omitted.Brief = true
	}
	for !fits() && len(c.Items) > 0 {
		c.Items = c.Items[:len(c.Items)-1]
		omitted.Items++
	}
	for !fits() && len(c.Meetings.Recent) > 0 {
		c.Meetings.Recent = c.Meetings.Recent[:len(c.Meetings.Recent)-1]
		omitted.Meetings++
	}
	for !fits() && len(c.Freshness.Sources) > 3 {
		c.Freshness.Sources = c.Freshness.Sources[:len(c.Freshness.Sources)-1]
		omitted.Sources++
	}
	return c, omitted
}
func PayloadBytes(card Card, format Format, affiliation string) (int, error) {
	if format == FormatMarkdown {
		return len(RenderMarkdown(card, affiliation)), nil
	}
	b, err := json.Marshal(card,
		json.FormatNilSliceAsNull(false),
		jsontext.AllowInvalidUTF8(true),
		json.Deterministic(true),
	)
	if err != nil {
		return 0, fmt.Errorf("encode person card: %w", err)
	}
	return len(b), nil
}

// BuildResult produces the shared wire envelope after enforcing the byte budget.
func BuildResult(ref string, card Card, opts Options) (PersonCardResult, error) {
	if opts.Format == "" {
		opts.Format = FormatJSON
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	card.affiliation = enum(opts.Affiliation, "internal", "external")
	card, omitted := Budget(card, opts.Format, opts.MaxBytes)
	size, err := PayloadBytes(card, opts.Format, card.affiliation)
	if err != nil {
		return PersonCardResult{}, err
	}
	if size > opts.MaxBytes {
		return PersonCardResult{}, ErrBudgetTooSmall
	}
	result := PersonCardResult{SchemaVersion: 1, Status: "ok", Ref: ref, AsOf: card.AsOf, CollectedAt: card.Freshness.CollectedAt, Format: opts.Format, ContentBytes: size, Truncated: omitted.Any(), Omitted: omitted, Freshness: card.Freshness, Gaps: card.Gaps, Candidates: []Candidate{}}
	if opts.Format == FormatMarkdown {
		content := RenderMarkdown(card, card.affiliation)
		result.Content = &content
	} else {
		result.Card = &card
	}
	return result, nil
}

// ResultForStatus returns an empty card with capped, quarantined candidates.
func ResultForStatus(status, ref string, collected time.Time, opts Options, candidates []Candidate, participantID *int64) PersonCardResult {
	if opts.Format == "" {
		opts.Format = FormatJSON
	}
	if opts.AsOf.IsZero() {
		opts.AsOf = collected
	}
	r := PersonCardResult{SchemaVersion: 1, Status: status, Ref: ref, AsOf: opts.AsOf.UTC(), CollectedAt: collected.UTC(), Format: opts.Format, Freshness: buildFreshness(FreshnessInputs{CollectedAt: collected}), Gaps: []string{}, Candidates: []Candidate{}}
	switch status {
	case "observed_only":
		r.Gaps = []string{"not_a_saved_profile"}
		r.ParticipantID = participantID
	case "unknown":
		r.Gaps = []string{"not_in_archive"}
	case "ambiguous":
		list := slices.Clone(candidates)
		slices.SortFunc(list, func(a, b Candidate) int {
			if a.PersonID < b.PersonID {
				return -1
			}
			if a.PersonID > b.PersonID {
				return 1
			}
			return 0
		})
		r.CandidatesMore = len(list) > 5
		for _, c := range list[:min(5, len(list))] {
			c.ContentTrust = "source_controlled"
			c.UntrustedText.DisplayName, c.UntrustedText.DisplayNameMore = clean(c.UntrustedText.DisplayName, 120)
			r.Candidates = append(r.Candidates, c)
		}
	}
	return r
}
