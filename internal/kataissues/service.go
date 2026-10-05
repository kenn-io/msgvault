// Package kataissues creates Kata issues that quote exact archive evidence
// and adds evidence to existing issues.
package kataissues

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

var (
	ErrInvalidRequest      = errors.New("invalid Kata issue request")
	ErrIdempotencyConflict = errors.New("idempotency key was used with a different request")
	ErrIssueDeleted        = errors.New("the issue this key filed was deleted in Kata or is not visible to this credential")
	ErrEvidenceLimit       = errors.New("evidence exceeds the request limit")
	ErrUnsupportedEvidence = errors.New("issue evidence metadata is not supported")
	ErrIssueChanged        = errors.New("kata issue kept changing")
	ErrIssueFull           = errors.New("kata issue quotes too many passages")
)

// RequestMetadataKey records the hash of the request that created an issue,
// so a retry under the same key can tell a replay from a different request.
const RequestMetadataKey = "msgvault.request"

type Archive interface {
	ArchiveUIDContext(ctx context.Context) (string, error)
}

type Kata interface {
	CreateTaskReused(ctx context.Context, project, idempotencyKey string, create taskclient.KataCreate) (taskclient.KataTask, bool, error)
	GetTask(ctx context.Context, project, taskID string) (taskclient.KataTask, error)
	FindActionTask(ctx context.Context, project, action string) (taskclient.KataTask, bool, error)
	FindMetadataTasks(ctx context.Context, project, key string, limit int) ([]taskclient.KataTask, error)
	MutateMetadataKey(ctx context.Context, project, taskID, key string, previous, value any) (taskclient.KataTask, error)
	MutateMetadataKeys(ctx context.Context, project, taskID, guardKey string, previous any, patch map[string]any) (taskclient.KataTask, error)
	AddComment(ctx context.Context, project, taskID, idempotencyKey, body string) error
}

type Evidence interface {
	Resolve(ctx context.Context, ref kataevidence.Reference) (kataevidence.Resolution, error)
}

type Service struct {
	Archive  Archive
	Kata     Kata
	Evidence Evidence
	People   personagenda.IdentityStore
	Project  string
}

type CreateInput struct {
	Title    string                   `json:"title"`
	Brief    string                   `json:"brief"`
	List     string                   `json:"list"`
	PersonID *int64                   `json:"person_id,omitzero"`
	Evidence []kataevidence.Reference `json:"evidence"`
}

// Result reports whether this call created the issue or found the one an
// earlier request under the same key created.
type Result struct {
	Issue    taskclient.KataTask
	Replayed bool
}

const maxLinkAttempts = 3

// MaxIssuePassages bounds the passages one issue quotes. At 1,000 runes of up
// to 4 bytes each plus its metadata entry, each passage adds about 5 KB to the
// issue Kata returns, so 64 stays well under the 1 MiB response limit.
const MaxIssuePassages = 64

// MaxIssueEntries bounds the evidence IDs one issue records, counting the
// extra IDs re-synced sources give passages it already quotes.
const MaxIssueEntries = 4 * MaxIssuePassages

// Create files one issue per Idempotency-Key. The issue carries a marker
// derived from the key, so a retry finds it in Kata (open or closed) before
// sending anything, even after Kata's own replay window has passed.
func (s *Service) Create(ctx context.Context, key string, input CreateInput) (Result, error) {
	input, err := normalizeCreate(input)
	if err != nil {
		return Result{}, err
	}
	hash := requestHash(input)
	archive, err := s.archiveUID(ctx)
	if err != nil {
		return Result{}, err
	}
	// The marker doubles as Kata's Idempotency-Key, so concurrent identical
	// creates also collapse into one issue inside Kata.
	marker := kataevidence.Digest([]string{archive, key})
	existing, found, err := s.Kata.FindActionTask(ctx, s.Project, marker)
	if err != nil {
		return Result{}, err
	}
	if found {
		return s.replay(existing, hash)
	}
	metadata := map[string]any{taskclient.ActionMetadataKey: marker, RequestMetadataKey: hash}
	if input.PersonID != nil {
		uids, err := personagenda.PersonUIDs(ctx, s.People, *input.PersonID)
		if err != nil {
			return Result{}, err
		}
		metadata[personagenda.PersonMetadataKey], metadata[personagenda.ListMetadataKey] = uids[0], input.List
	}
	quotes, err := s.resolve(ctx, input.Evidence)
	if err != nil {
		return Result{}, err
	}
	metadata[EvidenceMetadataKey] = Envelope{Version: 1, Entries: entriesOf(quotes)}
	for _, key := range sourceKeys(entriesOf(quotes)) {
		metadata[key] = true
	}
	issue, reused, err := s.Kata.CreateTaskReused(ctx, s.Project, marker, taskclient.KataCreate{Title: input.Title, Body: renderBody(input.Brief, firstOfEachPassage(quotes)), Metadata: metadata})
	if errors.Is(err, taskclient.ErrConflict) {
		return s.createConflict(ctx, marker, hash, taskclient.ErrorCode(err))
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Issue: issue, Replayed: reused}, nil
}

// createConflict reads Kata's refusal of a create. A key Kata already holds
// names the issue it filed, unless that issue was since deleted and no lookup
// returns it; any other conflict is Kata rejecting the request itself.
func (s *Service) createConflict(ctx context.Context, marker, hash, code string) (Result, error) {
	switch code {
	case "idempotency_deleted":
		return Result{}, ErrIssueDeleted
	case "idempotency_mismatch":
		existing, found, err := s.Kata.FindActionTask(ctx, s.Project, marker)
		switch {
		case err != nil:
			return Result{}, err
		case found:
			return s.replay(existing, hash)
		}
		return Result{}, ErrIssueDeleted
	}
	return Result{}, fmt.Errorf("%w: Kata refused the issue (%s)", taskclient.ErrRequestRejected, code)
}

// replay returns the issue this key filed when it was filed for the same
// request, and otherwise names it in a conflict.
func (s *Service) replay(existing taskclient.KataTask, hash string) (Result, error) {
	if recorded, _ := existing.Metadata[RequestMetadataKey].(string); recorded != hash {
		return Result{Issue: existing}, ErrIdempotencyConflict
	}
	return Result{Issue: existing, Replayed: true}, nil
}

// Link adds evidence to an existing issue. A guarded metadata write records
// each new passage first, marked pending with its quote, so provenance never
// depends on a comment landing and the cap counts it. Comments then quote the
// pending passages, and a second guarded write clears the marks. Any later
// Link to the issue posts and clears every still-pending passage. The comment key is the issue, passage and body, so a retry within
// Kata's 7-day replay window reuses the comment. Repeating a link changes nothing.
func (s *Service) Link(ctx context.Context, issueRef string, evidence []kataevidence.Reference) (taskclient.KataTask, error) {
	refs, err := canonicalReferences(evidence)
	if err != nil {
		return taskclient.KataTask{}, err
	}
	// A qualified ref names its project, such as an issue moved after filing;
	// Kata decides whether the caller may reach it.
	project := s.Project
	if named, short, qualified := strings.Cut(strings.TrimSpace(issueRef), "#"); qualified {
		project, issueRef = named, short
	}
	resolved := map[string]quotation{}
	for range maxLinkAttempts {
		current, envelope, err := s.issueEvidence(ctx, project, issueRef)
		if err != nil {
			return taskclient.KataTask{}, err
		}
		// passageOf holds every recorded evidence ID's passage; recorded marks
		// every passage the issue already quotes.
		passageOf, recorded := map[string]string{}, map[string]bool{}
		for _, entry := range envelope.Entries {
			passageOf[entry.ID], recorded[entry.Passage] = entry.Passage, true
		}
		var missing []kataevidence.Reference
		for _, ref := range refs {
			id := kataevidence.ID(ref)
			if _, ok := resolved[id]; !ok && passageOf[id] == "" {
				missing = append(missing, ref)
			}
		}
		quotes, err := s.resolve(ctx, missing)
		if err != nil {
			return taskclient.KataTask{}, err
		}
		for _, quote := range quotes {
			resolved[quote.ID] = quote
		}
		// A reference to a passage already quoted, such as one prepared again
		// after a re-sync, is recorded without a comment so a retry of the
		// same request recognizes it without reading the source.
		var entries []Entry
		added := 0
		for _, ref := range refs {
			id := kataevidence.ID(ref)
			if passageOf[id] != "" {
				continue
			}
			quote := resolved[id]
			entry := quote.Entry
			if !recorded[quote.Passage] {
				entry.Pending = &Pending{Quote: quote.Snapshot, Label: quote.Label}
				added++
			}
			passageOf[id], recorded[quote.Passage] = quote.Passage, true
			entries = append(entries, entry)
		}
		if len(entries) > 0 {
			if len(envelope.Entries)+len(entries) > MaxIssueEntries || added > 0 && distinctPassages(envelope.Entries)+added > MaxIssuePassages {
				// The passages already pending still get their comments.
				if _, err := s.postPending(ctx, project, current); err != nil {
					return taskclient.KataTask{}, err
				}
				return taskclient.KataTask{}, ErrIssueFull
			}
			merged := Envelope{Version: 1, Entries: append(slices.Clone(envelope.Entries), entries...)}
			patch := map[string]any{EvidenceMetadataKey: merged}
			for _, key := range sourceKeys(entries) {
				patch[key] = true
			}
			// Guarding only the evidence key lets unrelated edits land between attempts.
			updated, err := s.Kata.MutateMetadataKeys(ctx, project, current.UID, EvidenceMetadataKey, current.Metadata[EvidenceMetadataKey], patch)
			if guardFailed(err) {
				continue
			}
			if err != nil {
				return taskclient.KataTask{}, rejectedWrite(err)
			}
			current = updated
		}
		return s.postPending(ctx, project, current)
	}
	return taskclient.KataTask{}, ErrIssueChanged
}

// Citing returns up to limit issues, open or closed and oldest first, that
// carry a source key, and whether more do.
func (s *Service) Citing(ctx context.Context, key string, limit int) ([]taskclient.KataTask, bool, error) {
	issues, err := s.Kata.FindMetadataTasks(ctx, s.Project, key, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(issues) > limit {
		return issues[:limit], true, nil
	}
	return issues, false, nil
}

// issueEvidence reads an issue and the evidence it records.
func (s *Service) issueEvidence(ctx context.Context, project, issueRef string) (taskclient.KataTask, Envelope, error) {
	current, err := s.Kata.GetTask(ctx, project, issueRef)
	if errors.Is(err, taskclient.ErrWrongProject) && project != s.Project {
		return taskclient.KataTask{}, Envelope{}, taskclient.ErrNotFound
	}
	if err != nil {
		return taskclient.KataTask{}, Envelope{}, err
	}
	envelope := Envelope{Version: 1}
	if value, ok := current.Metadata[EvidenceMetadataKey]; ok {
		if envelope, err = parseEnvelope(value); err != nil {
			return taskclient.KataTask{}, Envelope{}, err
		}
	}
	return current, envelope, nil
}

// postPending posts the comment each pending passage on the issue still owes,
// in the order the entries were added, then clears their marks, so a link that
// failed partway is finished by the next one. Each clear is computed from the same snapshot its
// guard compares, so another link's entries survive it.
func (s *Service) postPending(ctx context.Context, project string, current taskclient.KataTask) (taskclient.KataTask, error) {
	for range maxLinkAttempts {
		value, ok := current.Metadata[EvidenceMetadataKey]
		if !ok {
			return current, nil
		}
		envelope, err := parseEnvelope(value)
		if err != nil {
			return taskclient.KataTask{}, err
		}
		cleared := slices.Clone(envelope.Entries)
		done, changed := map[string]bool{}, false
		for i, entry := range cleared {
			if entry.Pending == nil {
				continue
			}
			if !done[entry.Passage] {
				// Keying on the body too sends a new key when a label changed
				// between attempts, rather than a conflict that blocks the retry.
				body := renderQuote(quotation{Entry: entry, Snapshot: entry.Pending.Quote, Label: entry.Pending.Label})
				// Kata refuses a used key whose request changed, such as under a
				// new actor, so a mismatch means this comment already landed.
				err := s.Kata.AddComment(ctx, project, current.UID, kataevidence.Digest([]string{current.UID, entry.Passage, body}), body)
				if err != nil && taskclient.ErrorCode(err) != "idempotency_mismatch" {
					return taskclient.KataTask{}, err
				}
				done[entry.Passage] = true
			}
			cleared[i].Pending, changed = nil, true
		}
		if !changed {
			return current, nil
		}
		updated, err := s.Kata.MutateMetadataKey(ctx, project, current.UID, EvidenceMetadataKey, current.Metadata[EvidenceMetadataKey], Envelope{Version: 1, Entries: cleared})
		if !guardFailed(err) {
			return updated, rejectedWrite(err)
		}
		if current, _, err = s.issueEvidence(ctx, project, current.UID); err != nil {
			return taskclient.KataTask{}, err
		}
	}
	return taskclient.KataTask{}, ErrIssueChanged
}

// guardFailed reports whether a guarded metadata write lost to another edit,
// the one conflict worth reading the issue again for.
func guardFailed(err error) bool {
	return errors.Is(err, taskclient.ErrConflict) && taskclient.ErrorCode(err) == "metadata_guard_failed"
}

// rejectedWrite reports any other conflict as Kata refusing the write.
func rejectedWrite(err error) error {
	if errors.Is(err, taskclient.ErrConflict) {
		return fmt.Errorf("%w: Kata refused the evidence (%s)", taskclient.ErrRequestRejected, taskclient.ErrorCode(err))
	}
	return err
}

// firstOfEachPassage quotes each passage once even when several references cite it.
func firstOfEachPassage(quotes []quotation) []quotation {
	seen := map[string]bool{}
	return slices.DeleteFunc(slices.Clone(quotes), func(quote quotation) bool {
		duplicate := seen[quote.Passage]
		seen[quote.Passage] = true
		return duplicate
	})
}

// sourceKeys lists, once each, the source markers for what entries cite.
func sourceKeys(entries []Entry) []string {
	var keys []string
	for _, entry := range entries {
		keys = append(keys, kataevidence.SourceKeys(entry.Reference)...)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// distinctPassages counts the passages entries quote; several evidence IDs
// can share one.
func distinctPassages(entries []Entry) int {
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Passage] = true
	}
	return len(seen)
}

func (s *Service) archiveUID(ctx context.Context) (string, error) {
	archive, err := s.Archive.ArchiveUIDContext(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", kataevidence.ErrArchiveUnavailable, err)
	}
	return archive, nil
}

func (s *Service) resolve(ctx context.Context, refs []kataevidence.Reference) ([]quotation, error) {
	quotes := make([]quotation, 0, len(refs))
	for _, ref := range refs {
		resolved, err := s.Evidence.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		switch resolved.State {
		case kataevidence.Available:
		case kataevidence.Changed:
			return nil, kataevidence.ErrChanged
		case kataevidence.Unprocessed:
			return nil, kataevidence.ErrUnprocessed
		case kataevidence.Unsupported:
			return nil, kataevidence.ErrUnsupported
		default:
			return nil, kataevidence.ErrUnavailable
		}
		quotes = append(quotes, quotation{ID: resolved.Evidence.ID, Passage: resolved.Evidence.Passage, Reference: resolved.Evidence.Reference,
			Snapshot: resolved.Evidence.Excerpt, Label: label(resolved.Evidence.Display)})
	}
	return quotes, nil
}

func entriesOf(quotes []quotation) []Entry {
	entries := make([]Entry, len(quotes))
	for i, quote := range quotes {
		entries[i] = quote.Entry
	}
	return entries
}

func normalizeCreate(input CreateInput) (CreateInput, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.Brief = strings.TrimSpace(input.Brief)
	if input.Title == "" || !boundedText(input.Title, maxTitleChars) || !boundedText(input.Brief, maxBriefChars) {
		return CreateInput{}, ErrInvalidRequest
	}
	// A list names a section of a person's agenda, so it needs the person.
	if input.PersonID != nil && *input.PersonID < 1 || input.PersonID == nil && strings.TrimSpace(input.List) != "" {
		return CreateInput{}, ErrInvalidRequest
	}
	list, err := personagenda.NormalizeList(input.List)
	if err != nil {
		return CreateInput{}, ErrInvalidRequest
	}
	input.List = list
	input.Evidence, err = canonicalReferences(input.Evidence)
	return input, err
}

// canonicalReferences caps each request, not the issue's total, so a retry
// can always record a comment an earlier attempt already posted. It drops
// repeated evidence IDs and keeps the caller's order, which the issue body and
// comments follow; requestHash sorts for itself.
func canonicalReferences(refs []kataevidence.Reference) ([]kataevidence.Reference, error) {
	if len(refs) < 1 || len(refs) > kataevidence.MaxReferences {
		return nil, ErrEvidenceLimit
	}
	seen := make(map[string]bool, len(refs))
	result := make([]kataevidence.Reference, 0, len(refs))
	for _, ref := range refs {
		canonical, err := kataevidence.Canonicalize(ref)
		if err != nil {
			return nil, err
		}
		if id := kataevidence.ID(canonical); !seen[id] {
			seen[id] = true
			result = append(result, canonical)
		}
	}
	return result, nil
}

// requestHash identifies a create by its fields and where each citation
// sits, so a retry after a re-sync still matches the request that filed the
// issue.
func requestHash(input CreateInput) string {
	passages := make([]string, len(input.Evidence))
	for i, ref := range input.Evidence {
		passages[i] = kataevidence.PassageLocation(ref)
	}
	slices.Sort(passages)
	return kataevidence.Digest([]any{input.Title, input.Brief, input.List, input.PersonID, passages})
}
