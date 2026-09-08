package peoplesweep

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/personfacts"
)

// ParsedBrief is one validated brief: the structure that survived the drop
// rules, the evidence it cites in citation order, the attributes it proposes to
// the owner, and how many structured items Msgvault refused.
type ParsedBrief struct {
	Output   BriefOutput
	Evidence []personfacts.EvidenceInput
	// Claims holds validated suggestions for evaluation, not profile projection.
	Claims           []personfacts.ProposedClaim
	DroppedItemCount int
}

// ParseBrief mirrors ParseExtraction: it verifies the candidate against the
// frozen schema and the packet the provider actually saw, then enforces the
// brief's attribution rules in Msgvault rather than in the prompt.
//
// Two failure modes are deliberately different. A candidate that does not match
// the closed schema, or that proposes an attribute Msgvault cannot accept, is a
// validation failure the caller may repair once, exactly as for extraction. A
// structured item that cites unknown evidence or fails an attribution rule is
// dropped and counted, because the rest of the brief is still usable.
func ParseBrief(
	output json.RawMessage,
	window BriefWindow,
	profile ProviderProfile,
) (ParsedBrief, error) {
	packet, err := briefPacketFromWindow(window)
	if err != nil {
		return ParsedBrief{}, err
	}
	if err := validateBriefOutput(output); err != nil {
		return ParsedBrief{}, err
	}
	var candidate BriefOutput
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return ParsedBrief{}, errors.New("decode person brief output")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ParsedBrief{}, errors.New("person brief output contains trailing JSON")
	}

	evidence, err := extractionEvidenceByID(packet)
	if err != nil {
		return ParsedBrief{}, err
	}
	targets, err := packetTargetsByKey(packet)
	if err != nil {
		return ParsedBrief{}, err
	}
	claims, err := proposedClaims(
		candidate.PossibleAttributes, targets, evidence, profile, personfacts.OriginBrief)
	if err != nil {
		return ParsedBrief{}, err
	}

	parsed := ParsedBrief{Output: BriefOutput{PossibleAttributes: candidate.PossibleAttributes}}
	cited := newBriefCitations(evidence, window.Boundary)

	if candidate.LastMeaningfulInteraction != nil {
		interaction := *candidate.LastMeaningfulInteraction
		if items, ok := cited.resolve([]string{interaction.EvidenceID}); ok {
			cited.keep(items)
			parsed.Output.LastMeaningfulInteraction = &interaction
		} else {
			parsed.DroppedItemCount++
		}
	}

	// keptHighlights maps a submitted highlight index onto its index after
	// drops so a surviving follow-up still points at the highlight it explains.
	keptHighlights := make(map[int]int, len(candidate.Highlights))
	for index, highlight := range candidate.Highlights {
		items, ok := cited.resolve(highlight.EvidenceIDs)
		if !ok || !briefSpeakerSupported(highlight.Speaker, items, packet.PersonID) ||
			!cited.observedInWindow(highlight.ObservedAt) {
			parsed.DroppedItemCount++
			continue
		}
		cited.keep(items)
		keptHighlights[index] = len(parsed.Output.Highlights)
		parsed.Output.Highlights = append(parsed.Output.Highlights, highlight)
	}

	for _, followUp := range candidate.FollowUps {
		reindexed, referenced := keptHighlights[followUp.HighlightIndex]
		items, ok := cited.resolve(followUp.EvidenceIDs)
		if !referenced || !ok {
			parsed.DroppedItemCount++
			continue
		}
		cited.keep(items)
		followUp.HighlightIndex = reindexed
		parsed.Output.FollowUps = append(parsed.Output.FollowUps, followUp)
	}

	// Ruling R7: an appreciation is something the owner said, and no
	// owner-authored evidence can reach a v1 packet, so every appreciation is
	// unevidenced and is dropped rather than attributed to the person.
	parsed.DroppedItemCount += len(candidate.Appreciations)

	for _, uncertainty := range candidate.Uncertainties {
		items, ok := cited.resolve(uncertainty.EvidenceIDs)
		if !ok {
			parsed.DroppedItemCount++
			continue
		}
		cited.keep(items)
		parsed.Output.Uncertainties = append(parsed.Output.Uncertainties, uncertainty)
	}

	// A proposed attribute is a citation too, and it is the only one the loops
	// above never see. Keep its evidence pointer so the saved brief can show
	// the reader what the suggested attribute was read out of. It runs last
	// so the paragraph's own citations keep the
	// front of the order. proposedClaims already refused an unknown or
	// duplicate evidence ID, so resolve cannot fail here and a failure would
	// mean the claim list and the candidate had diverged.
	for _, attribute := range candidate.PossibleAttributes {
		items, ok := cited.resolve(attribute.EvidenceIDs)
		if !ok {
			return ParsedBrief{}, fmt.Errorf(
				"person brief attribute for target %q cites evidence the packet does not hold",
				attribute.TargetKey)
		}
		cited.keep(items)
	}

	parsed.Evidence, err = cited.inputs()
	if err != nil {
		return ParsedBrief{}, err
	}
	parsed.Claims = claims
	// structured_json is durable and re-validated against the frozen schema,
	// which requires arrays. A kind with no surviving item must serialize as []
	// rather than null.
	if parsed.Output.Highlights == nil {
		parsed.Output.Highlights = []BriefHighlight{}
	}
	if parsed.Output.FollowUps == nil {
		parsed.Output.FollowUps = []BriefFollowUp{}
	}
	if parsed.Output.Appreciations == nil {
		parsed.Output.Appreciations = []BriefAppreciation{}
	}
	if parsed.Output.Uncertainties == nil {
		parsed.Output.Uncertainties = []BriefUncertainty{}
	}
	if parsed.Output.PossibleAttributes == nil {
		parsed.Output.PossibleAttributes = []ExtractedClaim{}
	}
	return parsed, nil
}

// briefCitations resolves evidence IDs against the packet and records, in
// citation order, the items the surviving structure actually cites.
type briefCitations struct {
	byID     map[string]EvidenceItem
	boundary BriefBoundary
	order    []string
	seen     map[string]struct{}
}

func newBriefCitations(byID map[string]EvidenceItem, boundary BriefBoundary) *briefCitations {
	return &briefCitations{byID: byID, boundary: boundary, seen: make(map[string]struct{}, len(byID))}
}

// resolve reports the cited items, or false when any evidence ID is not in the
// packet the provider saw.
func (c *briefCitations) resolve(ids []string) ([]EvidenceItem, bool) {
	if len(ids) == 0 {
		return nil, false
	}
	items := make([]EvidenceItem, 0, len(ids))
	for _, id := range ids {
		item, exists := c.byID[id]
		if !exists {
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

func (c *briefCitations) keep(items []EvidenceItem) {
	for _, item := range items {
		id := packetEvidenceID(item)
		if _, exists := c.seen[id]; exists {
			continue
		}
		c.seen[id] = struct{}{}
		c.order = append(c.order, id)
	}
}

// observedInWindow checks an optional observation date against the window's
// event-time bounds at date granularity.
func (c *briefCitations) observedInWindow(observedAt *string) bool {
	if observedAt == nil {
		return true
	}
	observed, err := time.Parse(time.DateOnly, *observedAt)
	if err != nil {
		return false
	}
	from := c.boundary.FromEventTime.UTC().Format(time.DateOnly)
	through := c.boundary.ThroughEventTime.UTC().Format(time.DateOnly)
	day := observed.UTC().Format(time.DateOnly)
	return day >= from && day <= through
}

func (c *briefCitations) inputs() ([]personfacts.EvidenceInput, error) {
	inputs := make([]personfacts.EvidenceInput, 0, len(c.order))
	for _, id := range c.order {
		input, err := PersonFactEvidenceInput(c.byID[id])
		if err != nil {
			return nil, fmt.Errorf("person brief cites unaligned evidence %q: %w", id, err)
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

// briefSpeakerSupported enforces the brief's attribution boundary under ruling
// R7, which supersedes the design's directness-based gates.
//
// A v1 brief packet can only hold the person's own recent messages:
// personfacts.validateEvidenceInput requires the evidence subject to equal the
// person, and personSweepAuthorship sets a subject only for an authenticated
// sender. Directness therefore separates the person's chat messages
// (direct-self) from the person's meeting utterances (direct-other); it says
// nothing about the owner. So:
//
//   - person is supported by any packet item, which is the evidence-exists
//     check the packet already guarantees;
//   - owner is never supported, because no owner-authored evidence can reach a
//     packet, and an owner-attributed sentence would be unevidenced;
//   - other is supported only by an item whose subject is somebody else, which
//     keeps a partner's, child's, or colleague's fact off the person and is
//     fail-closed today.
//
// Appreciations are dropped for the same reason as owner highlights and are
// gated by the caller. Widening the evidence window to the owner's side of the
// conversation is a v2 program, not a change here.
func briefSpeakerSupported(speaker BriefSpeaker, items []EvidenceItem, personID int64) bool {
	switch speaker {
	case BriefSpeakerPerson:
		for _, item := range items {
			if item.SubjectPersonID != nil && *item.SubjectPersonID == personID &&
				(item.Directness == personfacts.DirectSelf ||
					item.Directness == personfacts.DirectOther) {
				return true
			}
		}
		return false
	case BriefSpeakerOther:
		for _, item := range items {
			if item.SubjectPersonID != nil && *item.SubjectPersonID != personID {
				return true
			}
		}
		return false
	case BriefSpeakerOwner:
		return false
	default:
		return false
	}
}

// briefPacketFromWindow mirrors extractionPacketFromBatch: the candidate is only
// meaningful against the exact packet bytes the provider was sent.
func briefPacketFromWindow(window BriefWindow) (EvidencePacket, error) {
	if window.Batch.Ordinal < 0 {
		return EvidencePacket{}, errors.New("person brief batch has an invalid ordinal")
	}
	packet, err := canonicalPacket(window.Batch.Packet)
	if err != nil {
		return EvidencePacket{}, fmt.Errorf("person brief batch packet is invalid: %w", err)
	}
	if packet.ProgramID != BriefProgramID || packet.ProgramVersion != BriefProgramVersion {
		return EvidencePacket{}, errors.New("person brief batch does not use the frozen brief program")
	}
	packetJSON, err := marshalPacketEnvelope(packet)
	if err != nil {
		return EvidencePacket{}, err
	}
	digest := sha256.Sum256(packetJSON)
	if window.Batch.InputHash != hex.EncodeToString(digest[:]) ||
		!reflect.DeepEqual(window.Batch.Request,
			briefStructuredRequest(packet, packetJSON, window.Request.MaxOutputTokens)) {
		return EvidencePacket{}, errors.New("person brief batch is not bound to its provider request")
	}
	return packet, nil
}

func validateBriefOutput(output json.RawMessage) error {
	var schema jsonschema.Schema
	if err := decodeSingleJSON(briefSchema, &schema); err != nil {
		return errors.New("frozen person brief schema is invalid")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return errors.New("frozen person brief schema cannot be resolved")
	}
	var decoded any
	if err := decodeJSONSchemaInstance(output, &decoded); err != nil {
		return errors.New("person brief output is not one JSON value")
	}
	if err := resolved.Validate(decoded); err != nil {
		return errors.New("person brief output does not match the closed schema")
	}
	return nil
}
