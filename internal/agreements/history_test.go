package agreements

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"hegel.dev/go/hegel"
)

func TestAcceptedTermsRequireOppositePartyAndSurviveProposal(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	terms := &Terms{Text: "Send the budget", Due: "2026-11-06", Obligor: "Alex", Beneficiary: "Sam"}
	history := []Event{{Input: validInput("request", "request", "beneficiary", "", 0, terms), Quotes: []string{"x"}}}
	view, err := project(history)
	must.NoError(err)
	check.Nil(view.Accepted)
	history = append(history, Event{Input: validInput("accept", "accept", "obligor", "request", 1, nil), Quotes: []string{"x"}})
	view, err = project(history)
	must.NoError(err)
	check.Equal(terms, view.Accepted)
	changed := *terms
	changed.Due = "2026-11-09"
	history = append(history, Event{Input: validInput("proposal", "propose", "obligor", "accept", 2, &changed), Quotes: []string{"x"}})
	view, err = project(history)
	must.NoError(err)
	check.Equal(terms, view.Accepted)
	history = append(history, Event{Input: validInput("replacement", "replace", "obligor", "proposal", 3, nil), Quotes: []string{"x"}})
	_, err = project(history)
	must.ErrorIs(err, ErrInvalid)
	history[3].Input.Actor = "beneficiary"
	view, err = project(history)
	must.NoError(err)
	check.Equal(&changed, view.Accepted)
	check.Equal("replacement", view.AcceptedEventID)
}

func validInput(id, action, actor, target string, count int, terms *Terms) Input {
	return Input{ID: id, Action: action, Actor: actor, Target: target, ExpectedEvents: count, Reviewed: true, Terms: terms,
		Evidence: []kataevidence.Reference{{Version: 1, Kind: "message", ArchiveUID: "archive", MessageID: int64(count + 1), SourceType: "email", SourceIdentifier: "owner@example.com", SourceMessageID: id,
			Message: &kataevidence.MessageReference{BodySHA256: strings.Repeat("a", 64), EndRune: 1}}}}
}

func TestHistoryBoundsAndStaleSiblingProposal(t *testing.T) {
	must := require.New(t)

	terms := &Terms{Text: "Deliver", Obligor: "Alex", Beneficiary: "Sam"}
	inputs := []Input{
		validInput("r", "request", "beneficiary", "", 0, terms),
		validInput("a", "accept", "obligor", "r", 1, nil),
		validInput("p1", "propose", "obligor", "a", 2, terms),
		validInput("p2", "propose", "beneficiary", "a", 3, terms),
		validInput("replace", "replace", "beneficiary", "p1", 4, nil),
	}
	var history []Event
	for _, input := range inputs {
		envelope, _, err := Append(history, input, []string{"x"})
		must.NoError(err)
		history = envelope.Events
	}
	_, _, err := Append(history, validInput("stale", "replace", "obligor", "p2", 5, nil), []string{"x"})
	must.ErrorIs(err, ErrInvalid)
	for len(history) < MaxEvents {
		id := strings.Repeat("p", len(history))
		envelope, _, err := Append(history, validInput(id, "propose", "obligor", "replace", len(history), terms), []string{"x"})
		must.NoError(err)
		history = envelope.Events
	}
	_, _, err = Append(history, validInput("full", "propose", "obligor", "replace", len(history), terms), []string{"x"})
	must.ErrorIs(err, ErrFull)
}

func TestAcceptanceMayIncludeTheProposalAlongsideNewAcceptanceEvidence(t *testing.T) {
	for _, initiator := range []string{"obligor", "beneficiary"} {
		t.Run(initiator, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)

			other := "obligor"
			if initiator == other {
				other = "beneficiary"
			}
			terms := &Terms{Text: "Deliver", Obligor: "Alex", Beneficiary: "Sam"}
			request := validInput("r", "request", initiator, "", 0, terms)
			envelope, _, err := Append(nil, request, []string{"x"})
			must.NoError(err)
			accept := validInput("a", "accept", other, "r", 1, nil)
			accept.Evidence = append(accept.Evidence, request.Evidence...)
			envelope, view, err := Append(envelope.Events, accept, []string{"x", "x"})
			must.NoError(err, "context does not cancel the independent acceptance citation")
			check.Equal(terms, view.Accepted)
			proposal := validInput("p", "propose", initiator, "a", 2, terms)
			envelope, _, err = Append(envelope.Events, proposal, []string{"x"})
			must.NoError(err)
			replace := validInput("x", "replace", other, "p", 3, nil)
			replace.Evidence = append(replace.Evidence, proposal.Evidence...)
			_, view, err = Append(envelope.Events, replace, []string{"x", "x"})
			must.NoError(err)
			check.Equal("x", view.AcceptedEventID)
		})
	}
}

func FuzzValidatedRequestPreservesExactTerms(f *testing.F) {
	f.Add("Send", "2026-11-06")
	f.Add("", "2026-02-30")
	f.Fuzz(func(t *testing.T, value, due string) {
		check := assert.New(t)
		must := require.New(t)

		input := validInput("r", "request", "beneficiary", "", 0, &Terms{Text: value, Due: due, Obligor: "Alex", Beneficiary: "Sam"})
		normal, err := Normalize(input)
		if err != nil {
			return
		}
		_, view, err := Append(nil, normal, []string{"x"})
		must.NoError(err)
		check.Nil(view.Accepted)
		check.Equal(value, view.History[0].Input.Terms.Text)
	})
}

// Draw the documented valid Terms domain, including escaping/control text and
// optional dates. Invalid whitespace-only labels are covered by validation tests.
func drawAgreementTerms(ht *hegel.T) Terms {
	ht.Helper()
	terms := Terms{
		Text:        hegel.Draw(ht, hegel.Text().MinSize(1).MaxSize(1000)),
		Obligor:     hegel.Draw(ht, hegel.Text().MinSize(1).MaxSize(128)),
		Beneficiary: hegel.Draw(ht, hegel.Text().MinSize(1).MaxSize(128)),
	}
	ht.Assume(strings.TrimSpace(terms.Text) != "" && strings.TrimSpace(terms.Obligor) != "" && strings.TrimSpace(terms.Beneficiary) != "" && terms.Obligor != terms.Beneficiary)
	if hegel.Draw(ht, hegel.Integers[int](0, 1)) == 1 {
		year := hegel.Draw(ht, hegel.Integers[int](0, 9999))
		month := time.Month(hegel.Draw(ht, hegel.Integers[int](1, 12)))
		lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
		day := hegel.Draw(ht, hegel.Integers[int](1, lastDay))
		terms.Due = time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	}
	return terms
}

func TestHegelProposalsPreserveLastAcceptedTerms(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		must := require.New(ht)
		roles := []string{"obligor", "beneficiary"}
		author := hegel.Draw(ht, hegel.Integers[int](0, 1))
		terms := drawAgreementTerms(ht)
		expected := terms
		history, _, err := Append(nil, validInput("r", "request", roles[author], "", 0, &terms), []string{"x"})
		must.NoError(err)
		history, _, err = Append(history.Events, validInput("a", "accept", roles[1-author], "r", 1, nil), []string{"x"})
		must.NoError(err)
		// All event slots are generated; the independent encoded-byte budget may
		// legitimately refuse a long sequence of maximal escaped terms earlier.
		proposals := hegel.Draw(ht, hegel.Integers[int](0, MaxEvents-2))
		for i := range proposals {
			proposed := drawAgreementTerms(ht)
			proposed.Obligor, proposed.Beneficiary = expected.Obligor, expected.Beneficiary
			role := roles[hegel.Draw(ht, hegel.Integers[int](0, 1))]
			input := validInput(fmt.Sprintf("p%d", i), "propose", role, "a", len(history.Events), &proposed)
			candidate := Envelope{Version: 1, Events: append(append([]Event{}, history.Events...), Event{Input: input, Quotes: []string{"x"}})}
			encoded, err := json.Marshal(candidate)
			must.NoError(err)
			next, view, err := Append(history.Events, input, []string{"x"})
			if len(encoded) > MaxEncodedBytes {
				must.ErrorIs(err, ErrFull)
				view, err = Decode(history)
				must.NoError(err)
				must.Equal(&expected, view.Accepted)
				must.Equal(&expected, view.History[0].Input.Terms)
				break
			}
			must.NoError(err)
			must.Equal(&expected, view.Accepted)
			must.Equal("a", view.AcceptedEventID)
			must.Equal(&expected, view.History[0].Input.Terms)
			history = next
		}
	}, hegel.WithTestCases(1000))
}

func TestHegelAcceptanceRequiresCounterparty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		must := require.New(ht)
		roles := []string{"obligor", "beneficiary"}
		for _, action := range []string{"accept", "replace"} {
			author := hegel.Draw(ht, hegel.Integers[int](0, 1))
			terms := drawAgreementTerms(ht)
			original := terms
			target := "r"
			expected := original
			history, _, err := Append(nil, validInput("r", "request", roles[author], "", 0, &terms), []string{"x"})
			must.NoError(err)
			if action == "replace" {
				history, _, err = Append(history.Events, validInput("a", "accept", roles[1-author], "r", 1, nil), []string{"x"})
				must.NoError(err)
				author = hegel.Draw(ht, hegel.Integers[int](0, 1))
				proposed := drawAgreementTerms(ht)
				proposed.Obligor, proposed.Beneficiary = original.Obligor, original.Beneficiary
				expected = proposed
				history, _, err = Append(history.Events, validInput("p", "propose", roles[author], "a", 2, &proposed), []string{"x"})
				must.NoError(err)
				target = "p"
			}
			_, _, err = Append(history.Events, validInput("x", action, roles[author], target, len(history.Events), nil), []string{"x"})
			must.ErrorIs(err, ErrInvalid)
			saved, view, err := Append(history.Events, validInput("x", action, roles[1-author], target, len(history.Events), nil), []string{"x"})
			must.NoError(err)
			must.Equal(&expected, view.Accepted)
			must.Equal("x", view.AcceptedEventID)
			must.Equal(&original, saved.Events[0].Input.Terms)
		}
	}, hegel.WithTestCases(1000))
}
