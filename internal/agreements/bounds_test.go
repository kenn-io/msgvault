package agreements

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func proposalInput(id string, count, citations int, terms *Terms) Input {
	input := validInput(id, "propose", "obligor", "a", count, terms)
	input.Evidence = nil
	for i := range citations {
		ref := validInput(fmt.Sprintf("%s-c%d", id, i), "propose", "obligor", "a", count, terms).Evidence[0]
		ref.MessageID = int64(10*count + i + 1)
		input.Evidence = append(input.Evidence, ref)
	}
	return input
}

func TestCitationLimitAccepts64AndRefuses65WithoutTrimming(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	terms := &Terms{Text: "Deliver", Obligor: "Alex", Beneficiary: "Sam"}
	history, _, err := Append(nil, validInput("r", "request", "beneficiary", "", 0, terms), []string{"x"})
	must.NoError(err)
	history, _, err = Append(history.Events, validInput("a", "accept", "obligor", "r", 1, nil), []string{"x"})
	must.NoError(err)
	for i := range 15 {
		input := proposalInput(fmt.Sprintf("p%d", i), len(history.Events), 4, terms)
		history, _, err = Append(history.Events, input, []string{"x", "x", "x", "x"})
		must.NoError(err)
	}
	// Two original citations + 15*4 + 2 = 64, before the event-count limit.
	input := proposalInput("p64", len(history.Events), 2, terms)
	history, view, err := Append(history.Events, input, []string{"x", "x"})
	must.NoError(err)
	check.Equal(terms, view.Accepted)
	_, _, err = Append(history.Events, proposalInput("p65", len(history.Events), 1, terms), []string{"x"})
	must.ErrorIs(err, ErrFull)
	check.Len(history.Events, 18)
}

func TestEncodedHistoryAcceptsExactByteBudgetAndRejectsOneMore(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	terms := &Terms{Text: "Deliver", Obligor: "Alex", Beneficiary: "Sam"}
	envelope := Envelope{Version: 1}
	for i := range 10 {
		input := proposalInput(fmt.Sprintf("p%d", i), i, 4, terms)
		if i == 0 {
			input = validInput("r", "request", "beneficiary", "", i, terms)
		}
		if i == 1 {
			input = validInput("a", "accept", "obligor", "r", i, nil)
		}
		quotes := make([]string, len(input.Evidence))
		for j := range input.Evidence {
			input.Evidence[j].SourceIdentifier = strings.Repeat("s", 4000)
			input.Evidence[j].Message.EndRune = 1000
			quotes[j] = strings.Repeat("🙂", 1000)
		}
		envelope.Events = append(envelope.Events, Event{Input: input, Quotes: quotes})
	}
	encoded, err := json.Marshal(envelope)
	must.NoError(err)
	must.Greater(len(encoded), MaxEncodedBytes)
	// Adjust opaque ASCII source labels to reach the documented byte bound,
	// retaining valid references, Unicode quote lengths and a valid event graph.
	excess := len(encoded) - MaxEncodedBytes
	for i := range envelope.Events {
		for j := range envelope.Events[i].Input.Evidence {
			ref := &envelope.Events[i].Input.Evidence[j]
			remove := min(excess, len(ref.SourceIdentifier)-1)
			ref.SourceIdentifier = ref.SourceIdentifier[:len(ref.SourceIdentifier)-remove]
			excess -= remove
		}
	}
	must.Zero(excess)
	encoded, err = json.Marshal(envelope)
	must.NoError(err)
	must.Len(encoded, MaxEncodedBytes)
	last := envelope.Events[len(envelope.Events)-1]
	_, view, err := Append(envelope.Events[:len(envelope.Events)-1], last.Input, last.Quotes)
	must.NoError(err)
	check.Equal(terms, view.Accepted)
	last.Input.Evidence[0].SourceIdentifier += "x"
	_, _, err = Append(envelope.Events[:len(envelope.Events)-1], last.Input, last.Quotes)
	must.ErrorIs(err, ErrFull)
}
