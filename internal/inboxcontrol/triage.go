package inboxcontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/emailtags"
)

// TriageInput contains explicit identities and untrusted classification evidence.
// Idempotency keys are bound at preview so apply cannot change replay identity.
type TriageInput struct {
	Source SourceIdentity    `json:"source"`
	Items  []TriageItemInput `json:"items" minItems:"1" maxItems:"100"`
}

type TriageItemInput struct {
	Target             Target   `json:"target"`
	Categories         []string `json:"categories" maxItems:"6"`
	EvidenceMessageIDs []int64  `json:"evidence_message_ids" maxItems:"100"`
	IdempotencyKey     string   `json:"idempotency_key,omitempty" maxLength:"128"`
}

type TriageProposalItem struct {
	Categories         []string `json:"categories" maxItems:"6"`
	EvidenceMessageIDs []int64  `json:"evidence_message_ids" maxItems:"100"`
	Classification     string   `json:"classification"`
	RetainInbox        bool     `json:"retain_inbox"`
	Request            Request  `json:"request"`
	Projected          State    `json:"projected"`
}

type TriageProposal struct {
	Source            SourceIdentity       `json:"source"`
	MappingRevision   int64                `json:"mapping_revision"`
	ArchiveRevision   string               `json:"archive_revision"`
	IncomingWatermark string               `json:"incoming_watermark"`
	Items             []TriageProposalItem `json:"items" minItems:"1" maxItems:"100"`
	IssuedAt          time.Time            `json:"issued_at"`
	ExpiresAt         time.Time            `json:"expires_at"`
	PreviewToken      string               `json:"preview_token"`
}

// TriageEvidenceStore reads membership only. Neither references nor classification
// input authorize content access, native writes, or additional tool execution.
type TriageEvidenceStore interface {
	ValidateInboxTriageEvidence(ctx context.Context, target Target, messageIDs []int64) error
}

func (input TriageInput) Validate() error {
	if input.Source.Validate() != nil || len(input.Items) < 1 || len(input.Items) > 100 {
		return ErrInvalid
	}
	seenTargets := map[Target]bool{}
	seenKeys := map[string]bool{}
	for _, item := range input.Items {
		if item.Target.Validate() != nil || seenTargets[item.Target] || len(item.IdempotencyKey) > 128 || !utf8.ValidString(item.IdempotencyKey) || (item.IdempotencyKey != "" && seenKeys[item.IdempotencyKey]) || len(item.Categories) > 6 || len(item.EvidenceMessageIDs) > 100 {
			return ErrInvalid
		}
		t := item.Target
		source := input.Source
		if t.SourceID != source.SourceID || t.SourceType != source.SourceType || t.SourceIdentifier != source.SourceIdentifier || t.AccountID != source.AccountID {
			return ErrDenied
		}
		seenTargets[t] = true
		seenKeys[item.IdempotencyKey] = true
		categories := map[string]bool{}
		for _, category := range item.Categories {
			if categories[category] || !validTriageCategory(category) {
				return ErrInvalid
			}
			categories[category] = true
		}
		seenEvidence := map[int64]bool{}
		for _, id := range item.EvidenceMessageIDs {
			if id <= 0 || seenEvidence[id] {
				return ErrInvalid
			}
			seenEvidence[id] = true
		}
	}
	return nil
}

func triageCategories(categories []string) ([]string, string) {
	active := canonicalSet(categories, false)
	if len(active) == 0 {
		active = []string{"uncertain"}
	}
	if len(active) > 1 {
		active = slices.DeleteFunc(active, func(category string) bool { return category == "finished" })
	}
	classification := active[0]
	if len(active) > 1 {
		classification = "conflicting"
	}
	return active, classification
}

func (s *Service) PreviewTriage(ctx context.Context, input TriageInput, principal Principal) (*TriageProposal, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	input.Items = slices.Clone(input.Items)
	keys := map[string]bool{}
	for _, item := range input.Items {
		if item.IdempotencyKey != "" {
			keys[item.IdempotencyKey] = true
		}
	}
	for i := range input.Items {
		for input.Items[i].IdempotencyKey == "" {
			var entropy [16]byte
			if _, err := rand.Read(entropy[:]); err != nil {
				return nil, ErrInternal
			}
			key := hex.EncodeToString(entropy[:])
			if !keys[key] {
				input.Items[i].IdempotencyKey = key
				keys[key] = true
			}
		}
	}
	store, ok := s.Ledger.(TriageSnapshotStore)
	evidence, hasEvidence := s.Ledger.(TriageEvidenceStore)
	if !ok || !hasEvidence || s.Resolve == nil || len(s.Key) < 32 {
		return nil, ErrUnavailable
	}
	sourceRequest := Request{Operation: OpGetCapabilities, Source: &input.Source, DryRun: true}
	if err := s.authorize(ctx, principal, sourceRequest); err != nil {
		return nil, err
	}
	targets := make([]Target, len(input.Items))
	for i, item := range input.Items {
		targets[i] = item.Target
		if err := s.authorize(ctx, principal, Request{Operation: OpGetState, Target: &targets[i], DryRun: true}); err != nil {
			return nil, err
		}
	}
	snapshot, err := store.InboxTriageSnapshot(ctx, input.Source, targets)
	if err != nil {
		return nil, safePreflightError(err)
	}
	if snapshot == nil || snapshot.Source != input.Source || len(snapshot.Candidates) != len(targets) || !validDigest(snapshot.ArchiveRevision) || !validDigest(snapshot.IncomingWatermark) {
		return nil, ErrUnavailable
	}
	if snapshot.MappingRevision <= 0 {
		return nil, ErrDenied
	}
	proposal := &TriageProposal{Source: input.Source, MappingRevision: snapshot.MappingRevision, ArchiveRevision: snapshot.ArchiveRevision, IncomingWatermark: snapshot.IncomingWatermark, Items: make([]TriageProposalItem, 0, len(targets))}
	var earliestExpiry time.Time
	for i, item := range input.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidate := snapshot.Candidates[i]
		if candidate.State.Target != item.Target || !candidate.Available {
			return nil, ErrUnavailable
		}
		observedCategories := slices.Clone(item.Categories)
		if slices.Contains(observedCategories, "finished") {
			for _, retained := range []string{"todo", "reply-needed", "watch", "delegated", "uncertain"} {
				if tag, ok := snapshot.Mappings[retained]; ok && emailtags.Contains(candidate.State.Tags, tag, input.Source.SourceType == sourceTypeIMAP) {
					observedCategories = append(observedCategories, retained)
				}
			}
		}
		categories, classification := triageCategories(observedCategories)
		add := make([]string, 0, len(categories))
		for _, category := range categories {
			id, ok := snapshot.Mappings[category]
			if !ok {
				return nil, ErrDenied
			}
			add = append(add, id)
		}
		normalized, err := emailtags.Normalize(emailtags.Change{Add: add}, input.Source.SourceType == sourceTypeIMAP)
		if err != nil {
			return nil, ErrUnavailable
		}
		add = normalized.Add
		slices.Sort(add)
		request := Request{Operation: OpTags, Target: &targets[i], Tags: &emailtags.Change{Add: add}, DryRun: true}
		if err := s.authorize(ctx, principal, request); err != nil {
			return nil, err
		}
		if err := evidence.ValidateInboxTriageEvidence(ctx, item.Target, item.EvidenceMessageIDs); err != nil {
			return nil, safePreflightError(err)
		}
		result, err := s.Control(ctx, request, principal, nil)
		if err != nil {
			return nil, err
		}
		if result == nil || result.Before == nil || result.Projected == nil || result.Before.Inbox == nil || !*result.Before.Inbox || result.Before.Read == nil || result.ExpiresAt.IsZero() {
			return nil, ErrUnavailable
		}
		if earliestExpiry.IsZero() || result.ExpiresAt.Before(earliestExpiry) {
			earliestExpiry = result.ExpiresAt
		}
		archivedHash, err := triageArchiveFingerprint(candidate.State)
		if err != nil {
			return nil, ErrUnavailable
		}
		liveHash, err := triageArchiveFingerprint(*result.Before)
		if err != nil {
			return nil, ErrUnavailable
		}
		if archivedHash != liveHash {
			return nil, ErrPlanChanged
		}
		// A provider's projection must preserve every marker and unrelated tag.
		preserved := *result.Before
		preserved.Tags = emailtags.Project(preserved.Tags, *request.Tags, input.Source.SourceType == sourceTypeIMAP)
		if input.Source.SourceType == sourceTypeIMAP {
			preserved.Flags = emailtags.Project(preserved.Flags, *request.Tags, true)
		}
		wantHash, err := SemanticFingerprint(preserved)
		if err != nil {
			return nil, ErrUnavailable
		}
		projectedHash, err := SemanticFingerprint(*result.Projected)
		if err != nil || wantHash != projectedHash {
			return nil, ErrUnavailable
		}
		request.DryRun = false
		request.Expected = result.Before
		// The apply path mints its internal controller token only after checking
		// the whole proposal inside the source lease and daemon write gate.
		request.PreviewToken = ""
		request.IdempotencyKey = item.IdempotencyKey
		proposal.Items = append(proposal.Items, TriageProposalItem{Categories: slices.Clone(item.Categories), EvidenceMessageIDs: slices.Clone(item.EvidenceMessageIDs), Classification: classification, RetainInbox: true, Request: request, Projected: *result.Projected})
	}
	tags, err := s.triageNativeCatalog(ctx, input.Source, principal)
	if err != nil {
		return nil, err
	}
	for _, item := range proposal.Items {
		for _, id := range item.Request.Tags.Add {
			matches := 0
			for _, tag := range tags {
				if emailtags.Contains([]string{tag.ID}, id, input.Source.SourceType == sourceTypeIMAP) {
					matches++
				}
			}
			if matches != 1 {
				return nil, ErrDenied
			}
		}
	}
	// Close read races before signing. No source gate is required because preview
	// has no writes; apply will revalidate these guards inside the write boundary.
	current, err := store.InboxTriageSnapshot(ctx, input.Source, targets)
	if err != nil {
		return nil, safePreflightError(err)
	}
	if current == nil || current.MappingRevision != snapshot.MappingRevision || current.ArchiveRevision != snapshot.ArchiveRevision || current.IncomingWatermark != snapshot.IncomingWatermark {
		return nil, ErrPlanChanged
	}
	if err := s.authorize(ctx, principal, sourceRequest); err != nil {
		return nil, err
	}
	for _, item := range proposal.Items {
		if err := s.authorize(ctx, principal, item.Request); err != nil {
			return nil, err
		}
	}
	proposal.IssuedAt = s.now()
	proposal.ExpiresAt = proposal.IssuedAt.Add(previewTTL)
	if earliestExpiry.Before(proposal.ExpiresAt) {
		proposal.ExpiresAt = earliestExpiry
	}
	if !proposal.ExpiresAt.After(proposal.IssuedAt) {
		return nil, ErrPlanChanged
	}
	hash, err := triageProposalFingerprint(*proposal)
	if err != nil {
		return nil, err
	}
	proposal.PreviewToken, err = SignPreview(s.Key, PreviewClaims{PrincipalID: principal.ID, IntentHash: hash, StateHash: hash, IssuedAt: proposal.IssuedAt, ExpiresAt: proposal.ExpiresAt})
	if err != nil {
		return nil, ErrInternal
	}
	return proposal, nil
}

func VerifyTriageProposal(key []byte, proposal TriageProposal, principal Principal, now time.Time) error {
	if proposal.Source.Validate() != nil || proposal.MappingRevision <= 0 || !validDigest(proposal.ArchiveRevision) || !validDigest(proposal.IncomingWatermark) || len(proposal.Items) < 1 || len(proposal.Items) > 100 {
		return ErrInvalid
	}
	input := TriageInput{Source: proposal.Source, Items: make([]TriageItemInput, len(proposal.Items))}
	for i, item := range proposal.Items {
		if item.Request.Operation != OpTags || item.Request.DryRun || item.Request.PreviewToken != "" || item.Request.Target == nil || item.Request.Expected == nil || item.Request.Expected.Target != *item.Request.Target || item.Request.Expected.Source != (SourceIdentity{}) || item.Request.Expected.ObservedAt.IsZero() || item.Request.Tags == nil || len(item.Request.Tags.Remove) != 0 || !item.RetainInbox {
			return ErrInvalid
		}
		intent := item.Request
		intent.DryRun = true
		intent.Expected = nil
		intent.IdempotencyKey = ""
		if intent.Validate() != nil {
			return ErrInvalid
		}
		input.Items[i] = TriageItemInput{Target: *item.Request.Target, Categories: item.Categories, EvidenceMessageIDs: item.EvidenceMessageIDs, IdempotencyKey: item.Request.IdempotencyKey}
	}
	if err := input.Validate(); err != nil {
		return err
	}
	hash, err := triageProposalFingerprint(proposal)
	if err != nil {
		return err
	}
	return VerifyPreview(key, proposal.PreviewToken, principal.ID, hash, hash, now)
}

func triageProposalFingerprint(proposal TriageProposal) (string, error) {
	proposal.PreviewToken = ""
	return fingerprint(struct {
		Domain   string
		Proposal TriageProposal
	}{"msgvault:inbox-triage:v1", proposal})
}

// Sync and live providers have different revision encodings (for example an
// IMAP SELECT includes NumMessages while sync stores UIDNEXT and MODSEQ). The
// archive guard and full live state are independently bound in the proposal;
// this comparison establishes that their common state markers still agree.
func triageArchiveFingerprint(state State) (string, error) {
	state.Revision = ""
	return SemanticFingerprint(state)
}

func (s *Service) triageNativeCatalog(ctx context.Context, source SourceIdentity, principal Principal) ([]emailtags.Tag, error) {
	request := Request{Operation: OpGetCapabilities, Source: &source, DryRun: true, NativeTagCatalog: true}
	if err := s.authorize(ctx, principal, request); err != nil {
		return nil, err
	}
	provider, err := s.Resolve(ctx, request)
	if err != nil {
		return nil, safePreflightError(err)
	}
	if provider == nil {
		return nil, ErrUnavailable
	}
	defer closeProvider(provider)
	catalog, ok := provider.(TagCatalogProvider)
	if !ok {
		return nil, ErrUnavailable
	}
	tags, err := catalog.TagCatalog(ctx, source)
	if err != nil {
		return nil, safePreflightError(err)
	}
	return tags, nil
}
