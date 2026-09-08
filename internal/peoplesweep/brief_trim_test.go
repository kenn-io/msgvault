package peoplesweep

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// briefTrimShape is the part of a trimmed structure the cap tests assert on:
// which items survived, in order, and where each follow-up still points.
type briefTrimShape struct {
	Interaction   bool
	Highlights    []string
	FollowUps     []int
	Uncertainties []string
}

func briefShape(output BriefOutput) briefTrimShape {
	shape := briefTrimShape{Interaction: output.LastMeaningfulInteraction != nil}
	for _, highlight := range output.Highlights {
		shape.Highlights = append(shape.Highlights, highlight.Text)
	}
	for _, followUp := range output.FollowUps {
		shape.FollowUps = append(shape.FollowUps, followUp.HighlightIndex)
	}
	for _, uncertainty := range output.Uncertainties {
		shape.Uncertainties = append(shape.Uncertainties, uncertainty.Text)
	}
	return shape
}

// briefTrimFixture is one brief with every trimmable kind. Its rendered
// sentence order is interaction, highlight 0, highlight 1, follow-up,
// uncertainty 0, uncertainty 1, and the trimmer drops from that tail, so the
// paragraph that survives any cap is a prefix of these six sentences.
func briefTrimFixture() ParsedBrief {
	return ParsedBrief{Output: BriefOutput{
		LastMeaningfulInteraction: &BriefInteraction{
			EvidenceID: "evidence:a", Summary: "you compared notes on the move",
		},
		Highlights: []BriefHighlight{
			{Text: "they packed the kitchen first", Speaker: BriefSpeakerPerson},
			{Text: "they start the new role in September", Speaker: BriefSpeakerPerson},
		},
		FollowUps: []BriefFollowUp{
			{Question: "how the move went", Why: "they were mid-move", HighlightIndex: 0},
		},
		Uncertainties: []BriefUncertainty{
			{Text: "the new city may have changed", Kind: BriefUncertaintyStale},
			{Text: "the autumn trip was never confirmed", Kind: BriefUncertaintyAmbiguous},
		},
	}}
}

// briefPrefix is the paragraph the first count sentences render to, which is
// exactly what a cap set to its rune length must leave behind.
func briefPrefix(t *testing.T, rendered RenderedBrief, count int) string {
	t.Helper()
	require.LessOrEqual(t, count, len(rendered.Sentences))
	texts := make([]string, 0, count)
	for _, sentence := range rendered.Sentences[:count] {
		texts = append(texts, sentence.Text)
	}
	return strings.Join(texts, " ")
}

func TestTrimRenderedBriefDropsFromTheTailUntilTheParagraphFits(t *testing.T) {
	full, err := RenderBrief(briefTrimFixture(), BriefWindow{})
	require.NoError(t, err)
	require.Len(t, full.Sentences, 6)

	tests := []struct {
		name string
		// keep is how many leading sentences the cap must leave. The cap itself
		// is that prefix's rune length, so the assertion is on the boundary.
		keep      int
		maxRunes  int
		wantShape briefTrimShape
	}{
		{
			name: "a paragraph inside the cap is untouched", keep: 6,
			wantShape: briefTrimShape{Interaction: true,
				Highlights:    []string{"they packed the kitchen first", "they start the new role in September"},
				FollowUps:     []int{0},
				Uncertainties: []string{"the new city may have changed", "the autumn trip was never confirmed"}},
		},
		{
			name: "the last uncertainty goes first", keep: 5,
			wantShape: briefTrimShape{Interaction: true,
				Highlights:    []string{"they packed the kitchen first", "they start the new role in September"},
				FollowUps:     []int{0},
				Uncertainties: []string{"the new city may have changed"}},
		},
		{
			name: "every uncertainty goes before any follow-up", keep: 4,
			wantShape: briefTrimShape{Interaction: true,
				Highlights: []string{"they packed the kitchen first", "they start the new role in September"},
				FollowUps:  []int{0}},
		},
		{
			name: "the follow-up goes before any highlight", keep: 3,
			wantShape: briefTrimShape{Interaction: true,
				Highlights: []string{"they packed the kitchen first", "they start the new role in September"}},
		},
		{
			name: "highlights go last, newest first", keep: 2,
			wantShape: briefTrimShape{Interaction: true,
				Highlights: []string{"they packed the kitchen first"}},
		},
		{
			name: "the last-interaction sentence is never dropped", keep: 1,
			wantShape: briefTrimShape{Interaction: true},
		},
		{
			name: "a cap below one sentence still keeps the interaction", keep: 1, maxRunes: 1,
			wantShape: briefTrimShape{Interaction: true},
		},
		{
			name: "a cap of zero disables trimming entirely", keep: 6, maxRunes: -1,
			wantShape: briefTrimShape{Interaction: true,
				Highlights:    []string{"they packed the kitchen first", "they start the new role in September"},
				FollowUps:     []int{0},
				Uncertainties: []string{"the new city may have changed", "the autumn trip was never confirmed"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			want := briefPrefix(t, full, test.keep)
			maxRunes := test.maxRunes
			if maxRunes == 0 {
				maxRunes = utf8.RuneCountInString(want)
			} else if maxRunes < 0 {
				maxRunes = 0
			}

			trimmed, rendered, err := trimRenderedBrief(briefTrimFixture(), BriefWindow{}, maxRunes)
			require.NoError(err)
			assert.Equal(want, rendered.Text)
			assert.Equal(test.wantShape, briefShape(trimmed.Output))
			assert.Equal(6-test.keep, trimmed.DroppedItemCount, "every trimmed item is counted")
			assert.Len(rendered.Sentences, test.keep)
		})
	}
}

// TestTrimRenderedBriefCountsRunesNotBytes pins the unit: a paragraph whose
// bytes exceed the cap but whose runes do not is left alone.
func TestTrimRenderedBriefCountsRunesNotBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	parsed := ParsedBrief{Output: BriefOutput{
		LastMeaningfulInteraction: &BriefInteraction{
			EvidenceID: "evidence:a", Summary: "вы обсудили переезд и осенние планы",
		},
		Uncertainties: []BriefUncertainty{
			{Text: "новый город мог измениться", Kind: BriefUncertaintyStale},
		},
	}}
	full, err := RenderBrief(parsed, BriefWindow{})
	require.NoError(err)
	runes := utf8.RuneCountInString(full.Text)
	require.Greater(len(full.Text), runes, "the fixture must be multibyte")

	trimmed, rendered, err := trimRenderedBrief(parsed, BriefWindow{}, runes)
	require.NoError(err)
	assert.Equal(full.Text, rendered.Text)
	assert.Zero(trimmed.DroppedItemCount)

	trimmed, rendered, err = trimRenderedBrief(parsed, BriefWindow{}, runes-1)
	require.NoError(err)
	assert.Equal(1, trimmed.DroppedItemCount)
	assert.Empty(trimmed.Output.Uncertainties)
	assert.Equal(utf8.RuneCountInString(rendered.Text), utf8.RuneCountInString(full.Sentences[0].Text))
}

func TestBriefDropLastHighlightTakesItsFollowUpsWithIt(t *testing.T) {
	tests := []struct {
		name        string
		output      BriefOutput
		wantDropped int
		wantShape   briefTrimShape
	}{
		{
			name: "a follow-up on the dropped highlight goes with it",
			output: BriefOutput{
				Highlights: []BriefHighlight{{Text: "kept"}, {Text: "dropped"}},
				FollowUps:  []BriefFollowUp{{Question: "a", HighlightIndex: 1}},
			},
			wantDropped: 2,
			wantShape:   briefTrimShape{Highlights: []string{"kept"}},
		},
		{
			name: "a follow-up on a surviving highlight keeps its index",
			output: BriefOutput{
				Highlights: []BriefHighlight{{Text: "kept"}, {Text: "dropped"}},
				FollowUps:  []BriefFollowUp{{Question: "a", HighlightIndex: 0}},
			},
			wantDropped: 1,
			wantShape:   briefTrimShape{Highlights: []string{"kept"}, FollowUps: []int{0}},
		},
		{
			name: "the sole highlight takes every follow-up with it",
			output: BriefOutput{
				Highlights: []BriefHighlight{{Text: "dropped"}},
				FollowUps: []BriefFollowUp{
					{Question: "a", HighlightIndex: 0}, {Question: "b", HighlightIndex: 0},
				},
			},
			wantDropped: 3,
			wantShape:   briefTrimShape{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trimmed, dropped := briefDropLastHighlight(test.output)
			assert.Equal(t, test.wantDropped, dropped)
			assert.Equal(t, test.wantShape, briefShape(trimmed))
			for _, followUp := range trimmed.FollowUps {
				assert.Less(t, followUp.HighlightIndex, len(trimmed.Highlights),
					"a surviving follow-up still names a highlight the reader can see")
			}
		})
	}
}

// TestTrimRenderedBriefNarrowsEvidenceToTheKeptItems proves the stored pointer
// list and the sentence-to-ordinal walk still agree after a trim: the evidence
// only the dropped uncertainty cited is gone, and the ordinals the API emits
// still account for exactly the pointers that remain.
func TestTrimRenderedBriefNarrowsEvidenceToTheKeptItems(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	parsed := briefCitationOutput(t)
	full, err := RenderBrief(parsed, BriefWindow{})
	require.NoError(err)
	require.Len(parsed.Evidence, 3)

	// Keeping only the interaction leaves one cited archive item, which is the
	// one the highlights, follow-up, and attribute no longer point at.
	runeCap := utf8.RuneCountInString(briefPrefix(t, full, 1))
	trimmed, rendered, err := trimRenderedBrief(parsed, BriefWindow{}, runeCap)
	require.NoError(err)
	assert.Equal(5, trimmed.DroppedItemCount, "one appreciation at parse plus four trimmed items")
	require.Len(rendered.Sentences, 1)

	// The proposed attribute is never trimmed, so its evidence stays alongside
	// the interaction's.
	require.Len(trimmed.Evidence, 2)
	refs := make([]int64, 0, len(trimmed.Evidence))
	for _, evidence := range trimmed.Evidence {
		ref, decodeErr := DecodePersonSweepEvidenceRef(evidence.SourceRef)
		require.NoError(decodeErr)
		refs = append(refs, ref.MessageID)
	}
	assert.Equal([]int64{12, 10}, refs)

	ordinals, ok := BriefCitationOrdinals(trimmed.Output, len(trimmed.Evidence))
	require.True(ok, "the trimmed structure still accounts for every stored pointer")
	assert.Equal(map[BriefSentenceKey][]int{
		{Kind: BriefSentenceLastInteraction, Index: 0}: {0},
	}, ordinals)
}
