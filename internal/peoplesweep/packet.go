package peoplesweep

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
)

const extractionMaxOutputTokens = 4096

type EvidencePacket struct {
	PersonID          int64
	ProgramID         string
	ProgramVersion    string
	Catalog           personfacts.Catalog
	CurrentProjection []ProjectedValue
	UnresolvedClaims  []personfacts.Claim
	Seeds             []EvidenceItem
	Context           []EvidenceItem
}

type PacketBatch struct {
	Ordinal   int
	InputHash string
	Request   StructuredRequest
	Packet    EvidencePacket
}

type ProjectedValue struct {
	TargetKey        string          `json:"target_key"`
	Value            json.RawMessage `json:"value"`
	ValueFingerprint string          `json:"value_fingerprint"`
	EffectiveAt      *time.Time      `json:"effective_at"`
}

type packetWireEnvelope struct {
	PersonID          int64                `json:"person_id"`
	ProgramID         string               `json:"program_id"`
	ProgramVersion    string               `json:"program_version"`
	Catalog           personfacts.Catalog  `json:"catalog"`
	CurrentProjection []ProjectedValue     `json:"current_projection"`
	UnresolvedClaims  []personfacts.Claim  `json:"unresolved_claims"`
	Seeds             []packetWireEvidence `json:"seeds"`
	Context           []packetWireEvidence `json:"context"`
}

type packetWireEvidence struct {
	ID                  string                         `json:"id"`
	SourceLane          SourceClass                    `json:"source_lane"`
	SourceRef           string                         `json:"source_ref"`
	PersonID            int64                          `json:"person_id"`
	SubjectPersonID     *int64                         `json:"subject_person_id"`
	SourceVersion       string                         `json:"source_version"`
	ContentSHA256       string                         `json:"content_sha256"`
	EventTime           string                         `json:"event_time"`
	RecordedTime        string                         `json:"recorded_time"`
	SpanStart           int                            `json:"span_start"`
	SpanEnd             int                            `json:"span_end"`
	Excerpt             string                         `json:"excerpt"`
	Provenance          personscope.Provenance         `json:"provenance"`
	IdentityBasisPoints int                            `json:"identity_basis_points"`
	Directness          personfacts.EvidenceDirectness `json:"directness"`
	Authority           personfacts.EvidenceAuthority  `json:"authority"`
}

func CanonicalPacketJSON(packet EvidencePacket) ([]byte, error) {
	canonical, err := canonicalPacket(packet)
	if err != nil {
		return nil, err
	}
	return marshalPacketEnvelope(canonical)
}

func PartitionEvidencePacket(
	packet EvidencePacket,
	maxBytes int,
	maxItems int,
) ([]PacketBatch, error) {
	if maxBytes <= 0 || maxItems <= 0 {
		return nil, errors.New("person sweep packet limits must be positive")
	}
	canonical, err := canonicalPacket(packet)
	if err != nil {
		return nil, err
	}
	if len(canonical.Seeds) == 0 {
		return nil, ErrNoChangedSeed
	}

	type evidenceParts struct {
		seeds   []EvidenceItem
		context []EvidenceItem
	}
	parts := make([]evidenceParts, 0, len(canonical.Seeds))
	for _, seed := range canonical.Seeds {
		if seed.Tombstone {
			continue
		}
		if len(parts) == 0 {
			parts = append(parts, evidenceParts{})
		}
		candidate := parts[len(parts)-1]
		candidate.seeds = append(candidate.seeds, seed)
		fits, fitErr := packetPartsFit(canonical, candidate.seeds, candidate.context, maxBytes, maxItems)
		if fitErr != nil {
			return nil, fitErr
		}
		if fits {
			parts[len(parts)-1] = candidate
			continue
		}
		candidate = evidenceParts{seeds: []EvidenceItem{seed}}
		fits, fitErr = packetPartsFit(canonical, candidate.seeds, nil, maxBytes, maxItems)
		if fitErr != nil {
			return nil, fitErr
		}
		if !fits {
			return nil, ErrEvidenceItemTooLarge
		}
		parts = append(parts, candidate)
	}
	if len(parts) == 0 {
		return nil, ErrNoChangedSeed
	}

	for _, item := range canonical.Context {
		if item.Tombstone {
			continue
		}
		placed := false
		for index := range parts {
			candidate := parts[index]
			candidate.context = append(candidate.context, item)
			fits, fitErr := packetPartsFit(canonical, candidate.seeds, candidate.context, maxBytes, maxItems)
			if fitErr != nil {
				return nil, fitErr
			}
			if fits {
				parts[index] = candidate
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		start := len(parts) % len(canonical.Seeds)
		for offset := range canonical.Seeds {
			anchor := canonical.Seeds[(start+offset)%len(canonical.Seeds)]
			candidate := evidenceParts{seeds: []EvidenceItem{anchor}, context: []EvidenceItem{item}}
			fits, fitErr := packetPartsFit(canonical, candidate.seeds, candidate.context, maxBytes, maxItems)
			if fitErr != nil {
				return nil, fitErr
			}
			if fits {
				parts = append(parts, candidate)
				placed = true
				break
			}
		}
		if !placed {
			return nil, ErrEvidenceItemTooLarge
		}
	}

	batches := make([]PacketBatch, 0, len(parts))
	for ordinal, part := range parts {
		batchPacket := canonical
		batchPacket.Seeds = part.seeds
		batchPacket.Context = part.context
		packetJSON, err := marshalPacketEnvelope(batchPacket)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(packetJSON)
		request := extractionStructuredRequest(batchPacket, packetJSON)
		if _, err := validateStructuredRequest(request, false); err != nil {
			return nil, fmt.Errorf("build person sweep structured request: %w", err)
		}
		batches = append(batches, PacketBatch{
			Ordinal: ordinal, InputHash: hex.EncodeToString(digest[:]), Request: request,
			Packet: batchPacket,
		})
	}
	return batches, nil
}

func extractionStructuredRequest(packet EvidencePacket, packetJSON []byte) StructuredRequest {
	return StructuredRequest{
		ProgramID: ExtractionProgramID, ProgramVersion: ExtractionProgramVersion,
		Sources:           packetSourceDescriptors(packet.Seeds, packet.Context),
		ContainsSensitive: packetContainsSensitive(packet),
		InputText:         extractionProgramText + "\n\nEvidence packet JSON:\n" + string(packetJSON),
		SchemaName:        ExtractionSchemaName, JSONSchema: ExtractionJSONSchema(),
		MaxOutputTokens: extractionMaxOutputTokens,
	}
}

func packetForProfile(packet EvidencePacket, profile ProviderProfile) (EvidencePacket, error) {
	canonical, err := canonicalPacket(packet)
	if err != nil {
		return EvidencePacket{}, err
	}
	eligible := make(map[string]struct{}, len(canonical.Catalog.Targets))
	targets := make([]personfacts.TargetDescriptor, 0, len(canonical.Catalog.Targets))
	for _, target := range canonical.Catalog.Targets {
		if target.Sensitive && !profile.AllowSensitive {
			continue
		}
		eligible[target.Key] = struct{}{}
		targets = append(targets, target)
	}
	canonical.Catalog.Targets = targets
	fingerprint, err := personfacts.CatalogFingerprint(targets)
	if err != nil {
		return EvidencePacket{}, fmt.Errorf("fingerprint policy-filtered person fact catalog: %w", err)
	}
	canonical.Catalog.Fingerprint = fingerprint
	canonical.CurrentProjection = slices.DeleteFunc(canonical.CurrentProjection, func(value ProjectedValue) bool {
		_, ok := eligible[value.TargetKey]
		return !ok
	})
	canonical.UnresolvedClaims = slices.DeleteFunc(canonical.UnresolvedClaims, func(claim personfacts.Claim) bool {
		_, ok := eligible[claim.Target.Key]
		return !ok
	})
	return canonical, nil
}

func canonicalPacket(packet EvidencePacket) (EvidencePacket, error) {
	if packet.PersonID <= 0 {
		return EvidencePacket{}, errors.New("person sweep packet requires a positive person ID")
	}
	if packet.ProgramID != ExtractionProgramID || packet.ProgramVersion != ExtractionProgramVersion {
		return EvidencePacket{}, errors.New("person sweep packet does not use the frozen extraction program")
	}
	canonical := packet
	canonical.Catalog.Targets = append([]personfacts.TargetDescriptor(nil), packet.Catalog.Targets...)
	seenTargets := make(map[string]struct{}, len(canonical.Catalog.Targets))
	for index := range canonical.Catalog.Targets {
		target := &canonical.Catalog.Targets[index]
		if target.Key == "" || target.Revision == "" || strings.TrimSpace(target.Description) == "" {
			return EvidencePacket{}, errors.New("person sweep packet contains an ineligible target")
		}
		if _, exists := seenTargets[target.Key]; exists {
			return EvidencePacket{}, fmt.Errorf("person sweep packet contains duplicate target %q", target.Key)
		}
		seenTargets[target.Key] = struct{}{}
		target.Choices = append([]personfacts.ChoiceDescriptor(nil), target.Choices...)
		sort.Slice(target.Choices, func(i, j int) bool {
			if target.Choices[i].Value != target.Choices[j].Value {
				return target.Choices[i].Value < target.Choices[j].Value
			}
			return target.Choices[i].Label < target.Choices[j].Label
		})
		target.Fields = append([]personfacts.FieldDescriptor(nil), target.Fields...)
	}
	sort.Slice(canonical.Catalog.Targets, func(i, j int) bool {
		return canonical.Catalog.Targets[i].Key < canonical.Catalog.Targets[j].Key
	})
	fingerprint, err := personfacts.CatalogFingerprint(canonical.Catalog.Targets)
	if err != nil {
		return EvidencePacket{}, fmt.Errorf("fingerprint person sweep packet catalog: %w", err)
	}
	if canonical.Catalog.Fingerprint != fingerprint {
		return EvidencePacket{}, errors.New("person sweep packet catalog fingerprint does not match targets")
	}

	canonical.CurrentProjection = append([]ProjectedValue(nil), packet.CurrentProjection...)
	for index := range canonical.CurrentProjection {
		value := &canonical.CurrentProjection[index]
		if _, ok := seenTargets[value.TargetKey]; !ok || value.ValueFingerprint == "" {
			return EvidencePacket{}, errors.New("person sweep packet projection has unknown target or fingerprint")
		}
		value.Value, err = canonicalRawJSON(value.Value)
		if err != nil {
			return EvidencePacket{}, fmt.Errorf("canonicalize projected value: %w", err)
		}
		if value.EffectiveAt != nil {
			effective := value.EffectiveAt.UTC()
			value.EffectiveAt = &effective
		}
	}
	sort.Slice(canonical.CurrentProjection, func(i, j int) bool {
		left, right := canonical.CurrentProjection[i], canonical.CurrentProjection[j]
		if left.TargetKey != right.TargetKey {
			return left.TargetKey < right.TargetKey
		}
		return left.ValueFingerprint < right.ValueFingerprint
	})

	canonical.UnresolvedClaims = append([]personfacts.Claim(nil), packet.UnresolvedClaims...)
	for index := range canonical.UnresolvedClaims {
		claim := &canonical.UnresolvedClaims[index]
		if claim.ClaimKey == "" {
			return EvidencePacket{}, errors.New("person sweep unresolved claim has no claim key")
		}
		if len(claim.SubmittedValue) > 0 {
			claim.SubmittedValue, err = canonicalRawJSON(claim.SubmittedValue)
			if err != nil {
				return EvidencePacket{}, fmt.Errorf("canonicalize unresolved claim: %w", err)
			}
		}
		claim.EvidenceIDs = append([]int64(nil), claim.EvidenceIDs...)
		slices.Sort(claim.EvidenceIDs)
	}
	sort.Slice(canonical.UnresolvedClaims, func(i, j int) bool {
		return canonical.UnresolvedClaims[i].ClaimKey < canonical.UnresolvedClaims[j].ClaimKey
	})

	canonical.Seeds, err = canonicalEvidenceItems(packet.Seeds)
	if err != nil {
		return EvidencePacket{}, fmt.Errorf("canonicalize person sweep seeds: %w", err)
	}
	canonical.Context, err = canonicalEvidenceItems(packet.Context)
	if err != nil {
		return EvidencePacket{}, fmt.Errorf("canonicalize person sweep context: %w", err)
	}
	if err := rejectAmbiguousEvidenceIDs(canonical.Seeds, canonical.Context); err != nil {
		return EvidencePacket{}, err
	}
	return canonical, nil
}

func rejectAmbiguousEvidenceIDs(groups ...[]EvidenceItem) error {
	seen := make(map[string][]byte)
	for _, items := range groups {
		wire, err := wireEvidenceItems(items)
		if err != nil {
			return err
		}
		for _, item := range wire {
			encoded, err := json.Marshal(item)
			if err != nil {
				return fmt.Errorf("encode person sweep evidence identity %q: %w", item.ID, err)
			}
			if prior, exists := seen[item.ID]; exists && !bytes.Equal(prior, encoded) {
				return fmt.Errorf("person sweep packet has ambiguous duplicate evidence ID %q", item.ID)
			}
			seen[item.ID] = encoded
		}
	}
	return nil
}

func canonicalEvidenceItems(items []EvidenceItem) ([]EvidenceItem, error) {
	canonical := append([]EvidenceItem(nil), items...)
	for index := range canonical {
		item := &canonical[index]
		if item.Tombstone {
			return nil, errors.New("provider packet cannot contain evidence tombstones")
		}
		if _, err := PersonFactEvidenceInput(*item); err != nil {
			return nil, err
		}
		item.SubjectPersonID = copySweepInt64(item.SubjectPersonID)
		item.EventTime = item.EventTime.UTC()
		item.RecordedTime = item.RecordedTime.UTC()
		item.Provenance.ParticipantIDs = append([]int64(nil), item.Provenance.ParticipantIDs...)
		item.Provenance.Roles = append([]personscope.Role(nil), item.Provenance.Roles...)
		item.Provenance.Directions = append([]personscope.Direction(nil), item.Provenance.Directions...)
		slices.Sort(item.Provenance.ParticipantIDs)
		slices.Sort(item.Provenance.Roles)
		slices.Sort(item.Provenance.Directions)
	}
	sort.Slice(canonical, func(i, j int) bool { return evidenceLess(canonical[i], canonical[j]) })
	return canonical, nil
}

func evidenceLess(left, right EvidenceItem) bool {
	if left.SourceClass != right.SourceClass {
		return left.SourceClass < right.SourceClass
	}
	if left.Ref.SourceID != right.Ref.SourceID {
		return left.Ref.SourceID < right.Ref.SourceID
	}
	if left.Ref.MessageID != right.Ref.MessageID {
		return left.Ref.MessageID < right.Ref.MessageID
	}
	if left.Ref.AttachmentID != right.Ref.AttachmentID {
		return left.Ref.AttachmentID < right.Ref.AttachmentID
	}
	if left.Ref.OccurrenceKey != right.Ref.OccurrenceKey {
		return left.Ref.OccurrenceKey < right.Ref.OccurrenceKey
	}
	if left.Ref.ChunkKey != right.Ref.ChunkKey {
		return left.Ref.ChunkKey < right.Ref.ChunkKey
	}
	if !left.EventTime.Equal(right.EventTime) {
		return left.EventTime.Before(right.EventTime)
	}
	return packetEvidenceID(left) < packetEvidenceID(right)
}

func marshalPacketEnvelope(packet EvidencePacket) ([]byte, error) {
	seeds, err := wireEvidenceItems(packet.Seeds)
	if err != nil {
		return nil, err
	}
	context, err := wireEvidenceItems(packet.Context)
	if err != nil {
		return nil, err
	}
	//nolint:musttag // Nested catalog and claim types own their canonical JSON shapes.
	encoded, err := json.Marshal(packetWireEnvelope{
		PersonID: packet.PersonID, ProgramID: packet.ProgramID, ProgramVersion: packet.ProgramVersion,
		Catalog: packet.Catalog, CurrentProjection: packet.CurrentProjection,
		UnresolvedClaims: packet.UnresolvedClaims, Seeds: seeds, Context: context,
	})
	if err != nil {
		return nil, fmt.Errorf("encode canonical person sweep packet: %w", err)
	}
	return encoded, nil
}

func wireEvidenceItems(items []EvidenceItem) ([]packetWireEvidence, error) {
	wire := make([]packetWireEvidence, 0, len(items))
	for _, item := range items {
		sourceRef, err := EncodePersonSweepEvidenceRef(item.Ref)
		if err != nil {
			return nil, err
		}
		wire = append(wire, packetWireEvidence{
			ID: packetEvidenceID(item), SourceLane: item.SourceClass, SourceRef: sourceRef,
			PersonID: item.PersonID, SubjectPersonID: copySweepInt64(item.SubjectPersonID),
			SourceVersion: item.SourceVersion, ContentSHA256: item.ContentSHA256,
			EventTime:    item.EventTime.UTC().Format(time.RFC3339Nano),
			RecordedTime: item.RecordedTime.UTC().Format(time.RFC3339Nano),
			SpanStart:    item.Highlight.Start, SpanEnd: item.Highlight.End,
			Excerpt: item.Excerpt, Provenance: item.Provenance,
			IdentityBasisPoints: item.IdentityBasisPoints,
			Directness:          item.Directness, Authority: item.Authority,
		})
	}
	return wire, nil
}

func packetEvidenceID(item EvidenceItem) string {
	ref, err := EncodePersonSweepEvidenceRef(item.Ref)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(ref + "\x00" + item.SourceVersion + "\x00" + item.ContentSHA256))
	return "evidence:" + hex.EncodeToString(digest[:])
}

func packetPartsFit(
	packet EvidencePacket,
	seeds []EvidenceItem,
	context []EvidenceItem,
	maxBytes int,
	maxItems int,
) (bool, error) {
	if len(seeds)+len(context) > maxItems {
		return false, nil
	}
	candidate := packet
	candidate.Seeds = seeds
	candidate.Context = context
	encoded, err := marshalPacketEnvelope(candidate)
	if err != nil {
		return false, err
	}
	inputBytes := len(extractionProgramText) + len("\n\nEvidence packet JSON:\n") + len(encoded)
	return inputBytes <= maxBytes, nil
}

func packetSourceDescriptors(seed, context []EvidenceItem) []SourceDescriptor {
	items := make([]EvidenceItem, 0, len(seed)+len(context))
	items = append(items, seed...)
	items = append(items, context...)
	seen := make(map[SourceDescriptor]struct{}, len(items))
	sources := make([]SourceDescriptor, 0, len(items))
	for _, item := range items {
		descriptor := SourceDescriptor{Class: item.SourceClass, ObservedOn: item.EventTime.UTC().Format(time.DateOnly)}
		if _, ok := seen[descriptor]; ok {
			continue
		}
		seen[descriptor] = struct{}{}
		sources = append(sources, descriptor)
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Class != sources[j].Class {
			return sources[i].Class < sources[j].Class
		}
		return sources[i].ObservedOn < sources[j].ObservedOn
	})
	return sources
}

func packetContainsSensitive(packet EvidencePacket) bool {
	// Archive excerpts are raw user content and may contain sensitive details
	// independently of the selected catalog targets. Keep provider egress
	// fail-closed unless the profile explicitly allows sensitive evidence.
	if len(packet.Seeds) > 0 || len(packet.Context) > 0 {
		return true
	}
	for _, target := range packet.Catalog.Targets {
		if target.Sensitive {
			return true
		}
	}
	return false
}

func canonicalRawJSON(value json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON value contains trailing data")
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
