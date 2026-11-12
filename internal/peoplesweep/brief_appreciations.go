package peoplesweep

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrBriefAppreciationMemoryInvalid reports that revalidated memory exceeds the
// supported active/resolved bounds, so the brief is refused before it is saved.
var ErrBriefAppreciationMemoryInvalid = errors.New("person brief appreciation memory exceeds supported state bounds")

// FilterBriefAppreciations rebuilds the in-flight structure, citation inputs,
// and deterministic paragraph after transactional source revalidation.
// The original last-contact header is retained because its date and channel
// came from the generation window rather than the structured model output.
// It refuses final memory that readers could not load, rather than evicting
// a supported entry.
func FilterBriefAppreciations(result *BriefResult, current map[string]BriefAppreciationMemory) error {
	if !ValidBriefAppreciationMemory(result.Boundary.AppreciationMemory) {
		return fmt.Errorf("%w: %w", ErrInvalidStructuredOutput, ErrBriefAppreciationMemoryInvalid)
	}
	var output BriefOutput
	if err := json.Unmarshal(result.Structured, &output); err != nil {
		return fmt.Errorf("decode appreciation brief: %w", err)
	}
	before := briefCitationOrder(output)
	kept := make([]BriefThanksSuggestion, 0, len(output.ThanksSuggestions))
	for _, suggestion := range output.ThanksSuggestions {
		if memory, ok := current[suggestion.Key]; ok {
			kept = append(kept, memory.Suggestion)
		}
	}
	output.ThanksSuggestions = kept
	result.Evidence = briefKeptEvidence(before, result.Evidence, output)
	parsed := ParsedBrief{Output: output}
	rendered, err := RenderBrief(parsed, BriefWindow{})
	if errors.Is(err, ErrEmptyBrief) {
		rendered = RenderedBrief{Policy: BriefRendererPolicyV3, Sentences: []RenderedSentence{}}
	} else if err != nil {
		return err
	}
	if len(rendered.Sentences) > 0 && rendered.Sentences[0].Kind == BriefSentenceLastInteraction &&
		len(result.Rendered.Sentences) > 0 && result.Rendered.Sentences[0].Kind == BriefSentenceLastInteraction {
		rendered.Sentences[0].Text = result.Rendered.Sentences[0].Text
		texts := make([]string, len(rendered.Sentences))
		for i := range rendered.Sentences {
			texts[i] = rendered.Sentences[i].Text
		}
		rendered.Text = strings.Join(texts, " ")
	}
	encoded, err := json.Marshal(output, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode appreciation brief: %w", err)
	}
	result.Structured, result.Rendered = encoded, rendered
	return nil
}

// BriefThanksSuggestion is a specific act backed by an exact peer quote.
// State and key are derived by the application, never accepted from the model.
type BriefThanksSuggestion struct {
	Text        string               `json:"text"`
	AnchorID    string               `json:"anchor_id"`
	Quote       string               `json:"quote"`
	Thanked     *BriefThanksEvidence `json:"thanked"`
	Key         string               `json:"key,omitempty"`
	State       string               `json:"state,omitempty"`
	PriorThanks string               `json:"prior_thanks,omitempty"`
}

type BriefThanksEvidence struct {
	EvidenceID string `json:"evidence_id"`
	Quote      string `json:"quote"`
}

// BriefAppreciationMemory freezes the validated association before rendering
// trims the paragraph. It holds references and hashes, not raw message bodies.
type BriefAppreciationMemory struct {
	Suggestion     BriefThanksSuggestion `json:"suggestion"`
	PeerRef        EvidenceRef           `json:"peer_ref"`
	PeerRawSHA256  string                `json:"peer_raw_sha256"`
	ConversationID int64                 `json:"conversation_id"`
	OwnerRef       *EvidenceRef          `json:"owner_ref,omitempty"`
	OwnerRawSHA256 string                `json:"owner_raw_sha256,omitempty"`
}

func freezeAppreciationMemory(parsed ParsedBrief, window BriefWindow) []BriefAppreciationMemory {
	memories := make([]BriefAppreciationMemory, 0, len(parsed.Output.ThanksSuggestions))
	for _, suggestion := range parsed.Output.ThanksSuggestions {
		var peer *BriefAppreciationPeer
		for i := range window.Batch.Packet.BriefPeers {
			if window.Batch.Packet.BriefPeers[i].AnchorID == suggestion.AnchorID {
				peer = &window.Batch.Packet.BriefPeers[i]
				break
			}
		}
		if peer == nil {
			continue
		}
		memory := BriefAppreciationMemory{Suggestion: suggestion, PeerRef: peer.Ref,
			PeerRawSHA256: peer.RawSHA256, ConversationID: peer.ConversationID}
		if suggestion.Thanked != nil {
			for _, owner := range window.Batch.Packet.BriefOwners {
				if owner.EvidenceID == suggestion.Thanked.EvidenceID {
					ref := owner.Ref
					memory.OwnerRef, memory.OwnerRawSHA256 = &ref, owner.RawSHA256
					break
				}
			}
		}
		memories = append(memories, memory)
	}
	return memories
}

// ValidBriefAppreciationMemory bounds active replay and per-generation proofs.
func ValidBriefAppreciationMemory(memory []BriefAppreciationMemory) bool {
	var active, resolved int
	for _, entry := range memory {
		switch entry.Suggestion.State {
		case "suggested":
			active++
		case "already_thanked":
			resolved++
		default:
			return false
		}
	}
	return active <= 3 && resolved <= 3
}

func retainedAppreciations(ctx context.Context, archive BriefArchive, request BriefWindowRequest, after, before *time.Time) ([]EvidenceItem, []BriefThanksSuggestion, error) {
	if request.Previous == nil || len(request.Previous.AppreciationMemory) == 0 {
		return nil, nil, nil
	}
	if !ValidBriefAppreciationMemory(request.Previous.AppreciationMemory) {
		return nil, nil, errors.New("person brief appreciation memory exceeds supported state bounds")
	}
	keys := make([]string, 0, len(request.Previous.AppreciationMemory))
	for _, memory := range request.Previous.AppreciationMemory {
		if memory.Suggestion.State == "suggested" {
			keys = append(keys, memory.Suggestion.Key)
		}
	}
	retired, err := archive.PersonSweepAppreciationRetired(ctx, request.PersonID, keys, request.Profile)
	if err != nil {
		return nil, nil, fmt.Errorf("read retained appreciation retirement: %w", err)
	}
	var retained []EvidenceItem
	var suggestions []BriefThanksSuggestion
	for _, memory := range request.Previous.AppreciationMemory {
		if memory.Suggestion.State != "suggested" || retired[memory.Suggestion.Key] {
			continue
		}
		items, err := archive.HydratePersonSweepMessages(ctx, request.PersonID, []int64{memory.PeerRef.MessageID}, request.ThroughSequence)
		if errors.Is(err, ErrPersonSweepMessageUnavailable) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("rehydrate retained appreciation: %w", err)
		}
		if len(items) != 1 || items[0].Ref != memory.PeerRef {
			continue
		}
		allowed, err := assemblyEvidenceAllowed(items[0], request.Profile, after, before)
		if err != nil {
			return nil, nil, err
		}
		if !allowed {
			continue
		}
		peers, err := archive.PersonSweepAppreciationPeers(ctx, request.PersonID, items, request.ThroughSequence)
		if err != nil {
			return nil, nil, err
		}
		if len(peers) != 1 || peers[0].ContributionKey != memory.Suggestion.Key || peers[0].RawSHA256 != memory.PeerRawSHA256 ||
			peers[0].ConversationID != memory.ConversationID || !strings.Contains(peers[0].ExactExcerpt, memory.Suggestion.Quote) {
			continue
		}
		suggestion := memory.Suggestion
		suggestion.AnchorID = packetEvidenceID(items[0])
		retained = append(retained, items[0])
		suggestions = append(suggestions, suggestion)
	}
	return retained, suggestions, nil
}

// BriefAppreciationPeer is raw-native evidence for one already admitted peer
// seed. Its exact text is separate from the normalized person-fact excerpt.
type BriefAppreciationPeer struct {
	AnchorID        string      `json:"anchor_id"`
	ContributionKey string      `json:"contribution_key"`
	Ref             EvidenceRef `json:"ref"`
	ConversationID  int64       `json:"conversation_id"`
	RawSHA256       string      `json:"raw_sha256"`
	ExactExcerpt    string      `json:"exact_excerpt"`
	EventTime       time.Time   `json:"event_time"`
}

// BriefAppreciationOwner is an independently authenticated owner message in a
// proven direct conversation. It is not person-fact evidence.
type BriefAppreciationOwner struct {
	EvidenceID     string      `json:"evidence_id"`
	Ref            EvidenceRef `json:"ref"`
	ConversationID int64       `json:"conversation_id"`
	RawSHA256      string      `json:"raw_sha256"`
	ExactExcerpt   string      `json:"exact_excerpt"`
	EventTime      time.Time   `json:"event_time"`
}
