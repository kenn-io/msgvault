package peoplesweep

import (
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/personfacts"
)

// BriefRendererPolicyV1 names the frozen wording of the deterministic renderer.
// It is stored on every brief row, so a later wording change becomes a new
// policy rather than a silent rewrite of what the owner already read.
const BriefRendererPolicyV1 = "person-brief-render-v1"

// briefDateFormat matches the design's rendered form ("Aug 29"). The structured
// record keeps full timestamps; the paragraph stays short.
const briefDateFormat = "Jan 2"

const (
	// briefDefaultMaxRenderedRunes is the default ceiling on the rendered
	// paragraph, in Unicode runes. The brief is meant to be read in the moment
	// before a call, so length is bounded in Go rather than asked for in the
	// prompt.
	briefDefaultMaxRenderedRunes = 560

	// briefMinRenderedRunes is the lowest cap an operator may configure. It is
	// the interaction summary's own schema maximum, which bounds the configured
	// value rather than guaranteeing the paragraph fits: the rendered sentence
	// prepends a header, so a summary at its schema maximum renders past 240. A
	// paragraph still over the cap once every droppable item is gone is stored
	// as it is, because the trimmer never cuts a sentence.
	briefMinRenderedRunes = 240
)

// ErrEmptyBrief reports that nothing survived validation, so there is no
// paragraph to store.
var ErrEmptyBrief = errors.New("person brief has no renderable content")

// BriefSentenceKind links one rendered sentence back to the structured item it
// came from, so a client can expand a sentence to its evidence.
type BriefSentenceKind string

const (
	BriefSentenceLastInteraction BriefSentenceKind = "last_interaction"
	BriefSentenceHighlight       BriefSentenceKind = "highlight"
	BriefSentenceFollowUp        BriefSentenceKind = "follow_up"
	BriefSentenceAppreciation    BriefSentenceKind = "appreciation"
	BriefSentenceUncertainty     BriefSentenceKind = "uncertainty"
)

// RenderedSentence is one sentence and the structured item that produced it.
// Index is the item's position within its kind in the validated structure.
type RenderedSentence struct {
	Kind  BriefSentenceKind `json:"kind"`
	Index int               `json:"index"`
	Text  string            `json:"text"`
}

// RenderedBrief is the paragraph a reader scans plus the sentence-to-item map.
type RenderedBrief struct {
	Policy    string             `json:"renderer_policy"`
	Text      string             `json:"text"`
	Sentences []RenderedSentence `json:"sentences"`
}

// RenderBrief turns a validated brief into one paragraph with deterministic Go.
// There is one sentence per structured item, in the order last interaction,
// highlights, follow-ups, appreciations, uncertainties.
//
// The header's date and channel come from person_contact_state through the
// window, because contact state is the deterministic answer to "when did we
// last talk"; the brief only answers "what about". When contact state names no
// usable message the header falls back to the cited item's event time and
// omits the excluded contact's channel.
func RenderBrief(parsed ParsedBrief, window BriefWindow) (RenderedBrief, error) {
	rendered := RenderedBrief{Policy: BriefRendererPolicyV1}

	if interaction := parsed.Output.LastMeaningfulInteraction; interaction != nil {
		header := "Last time you talked (" + briefHeaderDate(window, interaction.EvidenceID)
		if channel := strings.TrimSpace(window.LastContact.Channel); window.LastContact.Included && channel != "" {
			header += ", " + channel
		}
		rendered.Sentences = append(rendered.Sentences, RenderedSentence{
			Kind: BriefSentenceLastInteraction, Index: 0,
			Text: briefSentence(header + "): " + interaction.Summary),
		})
	}
	for index, highlight := range parsed.Output.Highlights {
		rendered.Sentences = append(rendered.Sentences, RenderedSentence{
			Kind: BriefSentenceHighlight, Index: index,
			Text: briefSentence(briefSpeakerPrefix(highlight.Speaker) + highlight.Text),
		})
	}
	for index, followUp := range parsed.Output.FollowUps {
		rendered.Sentences = append(rendered.Sentences, RenderedSentence{
			Kind: BriefSentenceFollowUp, Index: index,
			Text: briefSentence("You may want to ask " + followUp.Question),
		})
	}
	for index, appreciation := range parsed.Output.Appreciations {
		rendered.Sentences = append(rendered.Sentences, RenderedSentence{
			Kind: BriefSentenceAppreciation, Index: index,
			Text: briefSentence("You said you appreciated " + appreciation.Text),
		})
	}
	for index, uncertainty := range parsed.Output.Uncertainties {
		rendered.Sentences = append(rendered.Sentences, RenderedSentence{
			Kind: BriefSentenceUncertainty, Index: index,
			Text: briefSentence("Check before assuming: " + uncertainty.Text),
		})
	}
	if len(rendered.Sentences) == 0 {
		return RenderedBrief{}, ErrEmptyBrief
	}

	texts := make([]string, 0, len(rendered.Sentences))
	for _, sentence := range rendered.Sentences {
		texts = append(texts, sentence.Text)
	}
	rendered.Text = strings.Join(texts, " ")
	return rendered, nil
}

func briefSpeakerPrefix(speaker BriefSpeaker) string {
	switch speaker {
	case BriefSpeakerOwner:
		return "You said "
	case BriefSpeakerOther:
		return "Someone else said "
	case BriefSpeakerPerson:
		return "They said "
	default:
		return "They said "
	}
}

// briefHeaderDate prefers the deterministic last-contact time, then the event
// time of the cited item, then the end of the window.
func briefHeaderDate(window BriefWindow, evidenceID string) string {
	if window.LastContact.Included && !window.LastContact.EventTime.IsZero() {
		return briefFormatDate(window.LastContact.EventTime)
	}
	for _, item := range window.Items {
		if packetEvidenceID(item) == evidenceID {
			return briefFormatDate(item.EventTime)
		}
	}
	return briefFormatDate(window.Boundary.ThroughEventTime)
}

func briefFormatDate(value time.Time) string {
	return value.UTC().Format(briefDateFormat)
}

// briefSentence trims the model's text and terminates it, leaving punctuation
// the model already supplied.
func briefSentence(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasSuffix(trimmed, ".") || strings.HasSuffix(trimmed, "!") ||
		strings.HasSuffix(trimmed, "?") {
		return trimmed
	}
	return trimmed + "."
}

// BriefSentenceKey identifies one structured item by the kind and index the
// renderer stamps on the sentence it produces, so a rendered sentence and its
// citations can be joined without matching text.
type BriefSentenceKey struct {
	Kind  BriefSentenceKind
	Index int
}

// BriefCitationOrdinals rebuilds, for each structured item, the ordinals of the
// stored evidence pointers it cites.
//
// ParseBrief's traversal order is the contract this depends on, and the only
// join between a structured item and a pointer row. The parser records each
// surviving item's evidence IDs in first-seen order — the last interaction, the
// highlights, the follow-ups, the uncertainties, and finally the proposed
// attributes — and person_brief_evidence stores one pointer per distinct ID at
// exactly that position. Appreciations never survive a v1 parse (ruling R7), so
// they contribute no ordinal and get no entry here. Proposed attributes consume
// ordinals but have no sentence, so they are walked and not mapped. Change that
// order in ParseBrief and this walk has to change with it.
//
// pointerCount is how many pointer rows the version actually stored. The walk
// is accepted only when it produces exactly that many distinct ordinals: the
// store drops pointers when alignment fails, and a brief written by a later
// parser may cite differently, so a mismatch reports false and no mapping
// rather than pointing a sentence at the wrong archive item.
func BriefCitationOrdinals(
	output BriefOutput, pointerCount int,
) (map[BriefSentenceKey][]int, bool) {
	ordinals := make(map[string]int)
	byKey := make(map[BriefSentenceKey][]int)
	briefWalkCitations(output, func(key *BriefSentenceKey, ids []string) {
		cited := make([]int, 0, len(ids))
		for _, id := range ids {
			ordinal, seen := ordinals[id]
			if !seen {
				ordinal = len(ordinals)
				ordinals[id] = ordinal
			}
			if !slices.Contains(cited, ordinal) {
				cited = append(cited, ordinal)
			}
		}
		if key != nil {
			byKey[*key] = cited
		}
	})
	if len(ordinals) != pointerCount {
		return nil, false
	}
	return byKey, true
}

// briefWalkCitations visits every structured item's evidence IDs in the order
// ParseBrief records them: the last interaction, the highlights, the
// follow-ups, the uncertainties, and finally the proposed attributes. An
// attribute has no sentence, so it is visited with a nil key. This walk is the
// single definition of citation order; the ordinal map and the trimmer both
// read it, so neither can drift from the parser on its own.
func briefWalkCitations(output BriefOutput, visit func(key *BriefSentenceKey, ids []string)) {
	if interaction := output.LastMeaningfulInteraction; interaction != nil {
		visit(&BriefSentenceKey{Kind: BriefSentenceLastInteraction, Index: 0},
			[]string{interaction.EvidenceID})
	}
	for index, highlight := range output.Highlights {
		visit(&BriefSentenceKey{Kind: BriefSentenceHighlight, Index: index}, highlight.EvidenceIDs)
	}
	for index, followUp := range output.FollowUps {
		visit(&BriefSentenceKey{Kind: BriefSentenceFollowUp, Index: index}, followUp.EvidenceIDs)
	}
	for index, uncertainty := range output.Uncertainties {
		visit(&BriefSentenceKey{Kind: BriefSentenceUncertainty, Index: index}, uncertainty.EvidenceIDs)
	}
	for _, attribute := range output.PossibleAttributes {
		visit(nil, attribute.EvidenceIDs)
	}
}

// briefCitationOrder lists the distinct evidence IDs a structure cites, in the
// order ParseBrief records them. It is the position of an ID in this list that
// person_brief_evidence stores as its ordinal.
func briefCitationOrder(output BriefOutput) []string {
	seen := make(map[string]struct{})
	order := make([]string, 0)
	briefWalkCitations(output, func(_ *BriefSentenceKey, ids []string) {
		for _, id := range ids {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			order = append(order, id)
		}
	})
	return order
}

// briefSentenceCount is how many sentences a structure renders to. The trimmer
// uses it to refuse a drop that would leave no paragraph at all.
func briefSentenceCount(output BriefOutput) int {
	count := len(output.Highlights) + len(output.FollowUps) +
		len(output.Appreciations) + len(output.Uncertainties)
	if output.LastMeaningfulInteraction != nil {
		count++
	}
	return count
}

// trimRenderedBrief renders one validated brief and drops structured items from
// the tail until the paragraph fits maxRunes Unicode runes.
//
// Items are sacrificed in the order uncertainties, follow-ups, highlights, each
// last one first, so the newest and most concrete material survives longest.
// The last-interaction sentence is never dropped, and neither is the final
// remaining sentence: the paragraph may exceed the cap rather than be truncated
// mid-sentence. Every dropped item is added to DroppedItemCount, the returned
// structure is the trimmed one that gets stored, and the evidence list is
// rebuilt in citation order over the kept items so a stored pointer ordinal
// still names the archive item its sentence cites.
//
// A maxRunes of zero or less leaves the paragraph alone, which is what a caller
// that never configured a cap gets.
func trimRenderedBrief(
	parsed ParsedBrief, window BriefWindow, maxRunes int,
) (ParsedBrief, RenderedBrief, error) {
	rendered, err := RenderBrief(parsed, window)
	if err != nil {
		return ParsedBrief{}, RenderedBrief{}, err
	}
	if maxRunes <= 0 || utf8.RuneCountInString(rendered.Text) <= maxRunes {
		return parsed, rendered, nil
	}
	before := briefCitationOrder(parsed.Output)
	for utf8.RuneCountInString(rendered.Text) > maxRunes {
		trimmed, dropped, ok := briefDropTrailingItem(parsed.Output)
		if !ok {
			break
		}
		parsed.Output = trimmed
		parsed.DroppedItemCount += dropped
		if rendered, err = RenderBrief(parsed, window); err != nil {
			return ParsedBrief{}, RenderedBrief{}, err
		}
	}
	parsed.Evidence = briefKeptEvidence(before, parsed.Evidence, parsed.Output)
	return parsed, rendered, nil
}

// briefKeptEvidence narrows the citation-ordered evidence to the items the
// trimmed structure still cites, keeping citation order.
//
// before is the citation order of the structure the parser produced, which is
// positionally aligned with evidence. When that alignment does not hold the
// full list is returned unchanged: a pointer set that covers more than the
// paragraph is readable, while a misaligned one would point a sentence at the
// wrong archive item.
func briefKeptEvidence(
	before []string, evidence []personfacts.EvidenceInput, trimmed BriefOutput,
) []personfacts.EvidenceInput {
	if len(before) != len(evidence) {
		return evidence
	}
	positions := make(map[string]int, len(before))
	for position, id := range before {
		positions[id] = position
	}
	after := briefCitationOrder(trimmed)
	kept := make([]personfacts.EvidenceInput, 0, len(after))
	for _, id := range after {
		position, exists := positions[id]
		if !exists {
			return evidence
		}
		kept = append(kept, evidence[position])
	}
	return kept
}

// briefDropTrailingItem removes the least valuable remaining structured item
// and reports how many items went with it.
//
// Dropping a highlight also drops the follow-ups that explain it and reindexes
// the ones that survive, because a follow-up's highlight_index is a position in
// the highlight list and an orphaned follow-up would name a highlight the
// reader cannot see. It reports false when only one sentence is left, so a
// brief is never trimmed away to nothing.
func briefDropTrailingItem(output BriefOutput) (BriefOutput, int, bool) {
	if briefSentenceCount(output) < 2 {
		return output, 0, false
	}
	switch {
	case len(output.Uncertainties) > 0:
		output.Uncertainties = slices.Clone(output.Uncertainties[:len(output.Uncertainties)-1])
		return output, 1, true
	case len(output.FollowUps) > 0:
		output.FollowUps = slices.Clone(output.FollowUps[:len(output.FollowUps)-1])
		return output, 1, true
	case len(output.Highlights) > 0:
		trimmed, dropped := briefDropLastHighlight(output)
		return trimmed, dropped, true
	default:
		return output, 0, false
	}
}

// briefDropLastHighlight removes the final highlight together with every
// follow-up that explains it, and reindexes the follow-ups that survive.
//
// A follow-up's highlight_index is a position in the highlight list, so a
// follow-up left behind would name a highlight the reader cannot see and a
// follow-up after the dropped one would name the wrong highlight entirely. The
// trimmer reaches highlights only once the follow-ups are gone, so the cascade
// is the invariant that keeps that ordering from being load-bearing.
func briefDropLastHighlight(output BriefOutput) (BriefOutput, int) {
	last := len(output.Highlights) - 1
	if last < 0 {
		return output, 0
	}
	output.Highlights = slices.Clone(output.Highlights[:last])
	kept := make([]BriefFollowUp, 0, len(output.FollowUps))
	dropped := 1
	for _, followUp := range output.FollowUps {
		if followUp.HighlightIndex == last {
			dropped++
			continue
		}
		if followUp.HighlightIndex > last {
			followUp.HighlightIndex--
		}
		kept = append(kept, followUp)
	}
	output.FollowUps = kept
	return output, dropped
}
