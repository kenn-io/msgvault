package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// ErrNoBriefEvidence reports that the person has no archive evidence inside the
// profile's lanes and dates, so there is nothing to summarize.
var ErrNoBriefEvidence = errors.New("person brief window has no eligible evidence")

// briefCandidateHeadroom multiplies max_items when listing candidate messages.
// The listing already keeps only messages the person authored, which is the
// brief's eligibility rule, but a candidate is still a message row: some carry
// no durable text and hydrate to nothing, so the window asks for headroom and
// caps the hydrated items itself.
const briefCandidateHeadroom = 4

// briefMaxCandidates bounds the candidate scan regardless of configuration.
const briefMaxCandidates = 2000

// briefLanes are the source lanes a brief can summarize. In this version that
// is conversation_text alone. meeting_text is excluded until meeting
// transcripts carry speaker attribution: the meeting importers do not
// authenticate who said what, so a meeting utterance can never be evidence the
// person is the subject of under the current contract, and a lane that admits
// no item would only advertise evidence the window cannot hold. document_text
// has no event-time listing path: person sweep document retrieval is a lexical
// search that requires a query, and the brief window has none.
var briefLanes = []SourceClass{SourceConversationText}

// BriefArchive is the archive access one brief window needs. It extends the
// context archive with the direct hydration used to pull the deterministic
// last-contact message into the window. Every read takes the window's
// ThroughSequence so the boundary the brief records describes exactly what it
// saw: a message journaled after that sequence is not part of this window.
type BriefArchive interface {
	ContextArchive
	// HydratePersonSweepMessages reads the named messages, refusing one the
	// person's change journal first recorded after throughSequence. Zero
	// leaves the read unbounded.
	HydratePersonSweepMessages(
		ctx context.Context, personID int64, messageIDs []int64, throughSequence int64,
	) ([]EvidenceItem, error)
	PersonSweepLastContact(ctx context.Context, personID int64) (BriefLastContact, bool, error)
}

// BriefLastContact is the deterministic contact-state coordinate the brief must
// agree with. It carries identifiers only, never archive text.
type BriefLastContact struct {
	MessageID int64
	SourceID  int64
	Channel   string
}

// BriefWindowLastContact reports what the window did with the deterministic
// last contact. Included is false when contact state names no message, or when
// the named message is gone, out of person scope, or outside the profile's
// allowed lanes and dates.
type BriefWindowLastContact struct {
	BriefLastContact

	Included  bool
	EventTime time.Time
}

// BriefWindowRequest bounds one brief evidence window.
type BriefWindowRequest struct {
	PersonID int64
	Catalog  personfacts.Catalog
	Profile  ProviderProfile

	MaxItems     int
	MaxBytes     int
	OverlapItems int

	// MaxOutputTokens lowers the frozen program's output cap. Zero keeps it.
	MaxOutputTokens int64

	// MaxRenderedRunes caps the rendered paragraph in Unicode runes. The
	// renderer drops structured items from the tail until the paragraph fits.
	// Zero leaves the paragraph uncapped.
	MaxRenderedRunes int

	// ThroughSequence is the archive commit sequence the window ends at. It
	// bounds every retrieval, so a message the person's journal recorded after
	// it never enters the window, and it is recorded on the boundary so the
	// next run can tell whether new activity arrived since this brief. Zero
	// leaves retrieval unbounded and is only meaningful before the journal has
	// ever committed.
	ThroughSequence int64

	// Previous is the boundary of the person's current brief, when one exists.
	// Items at or before its through_event_time were already summarized and are
	// admitted only up to OverlapItems, so an open thread can be recognized as
	// continued or resolved without paying for the whole previous window again.
	Previous *BriefBoundary

	// BatchOrdinal is the provider call coordinate the brief batch occupies. It
	// follows the attempt's extraction batches.
	BatchOrdinal int
}

// BriefBoundary is the durable description of one brief's input. It is stored
// verbatim as person_briefs.boundary_json.
type BriefBoundary struct {
	Lanes            []string  `json:"lanes"`
	FromEventTime    time.Time `json:"from_event_time"`
	ThroughEventTime time.Time `json:"through_event_time"`
	ThroughSequence  int64     `json:"through_sequence"`
	ItemCount        int       `json:"item_count"`
	InputBytes       int       `json:"input_bytes"`
	PacketSHA256     string    `json:"packet_sha256"`
}

// Canonical returns the boundary with sorted lanes and UTC times truncated to
// the second, which is the form serialized into boundary_json.
func (b BriefBoundary) Canonical() BriefBoundary {
	canonical := b
	canonical.Lanes = append([]string(nil), b.Lanes...)
	slices.Sort(canonical.Lanes)
	canonical.FromEventTime = b.FromEventTime.UTC().Truncate(time.Second)
	canonical.ThroughEventTime = b.ThroughEventTime.UTC().Truncate(time.Second)
	return canonical
}

// BriefWindow is one deterministic brief input: the canonical packet bound to
// its provider request, the boundary that describes it, and the items it holds
// in newest-first order.
type BriefWindow struct {
	Request     BriefWindowRequest
	Batch       PacketBatch
	Boundary    BriefBoundary
	Items       []EvidenceItem
	LastContact BriefWindowLastContact
}

// BuildBriefWindow retrieves the most recent stretch of communication with one
// person and freezes it into a canonical, sensitive packet. Unlike extraction,
// which works from journal deltas because it wants changes, the brief wants a
// window: newest-first within the profile's lanes and dates, bounded by item
// and byte caps, always including the deterministic last-contact item, and
// overlapping the previous brief by at most overlap_items.
func BuildBriefWindow(
	ctx context.Context, archive BriefArchive, request BriefWindowRequest,
) (BriefWindow, error) {
	if archive == nil {
		return BriefWindow{}, errors.New("person brief window requires an archive")
	}
	if request.PersonID <= 0 || request.MaxItems <= 0 || request.MaxBytes <= 0 ||
		request.OverlapItems < 0 || request.ThroughSequence < 0 || request.BatchOrdinal < 0 {
		return BriefWindow{}, errors.New("person brief window limits are invalid")
	}
	lanes := briefWindowLanes(request.Profile.AllowedSources)
	if len(lanes) == 0 {
		return BriefWindow{}, errors.New("person brief window has no allowed source lane")
	}
	after, before, err := contextDateBounds(request.Profile.SourceSince, request.Profile.SourceUntil)
	if err != nil {
		return BriefWindow{}, err
	}

	// The brief admits only evidence the person is the subject of, which
	// hydration grants to a message they sent on a source that authenticates
	// its sender. The listing applies that rule itself: otherwise a
	// conversation where the owner writes most of the messages fills the
	// candidate limit with rows the window would drop, and the person's own
	// older messages never enter it.
	candidateLimit := min(request.MaxItems*briefCandidateHeadroom, briefMaxCandidates)
	candidates, err := archive.ListPersonSweepHistoricalCandidates(ctx, HistoricalCandidateRequest{
		PersonID: request.PersonID, SourceClasses: lanes,
		SourceSince: request.Profile.SourceSince, SourceUntil: request.Profile.SourceUntil,
		Limit: candidateLimit, ThroughSequence: request.ThroughSequence,
		AuthoredByPerson: true,
	})
	if err != nil {
		return BriefWindow{}, fmt.Errorf("list person brief candidates: %w", err)
	}
	// The search treats an empty candidate list as "list your own", and that
	// listing does not apply the authorship filter, so passing one through
	// would hydrate the whole unfiltered scope for a person who authored
	// nothing on an authenticating source, such as an email-only contact,
	// only to reject every row. No candidate means no evidence.
	if len(candidates) == 0 {
		return BriefWindow{}, ErrNoBriefEvidence
	}
	// An empty target carries no lexical filter, so the search returns every
	// hydrated candidate in newest-first candidate order. The window applies its
	// own caps below rather than borrowing the retrieval limit.
	retrieved, err := archive.SearchPersonSweepMessages(ctx, ContextRequest{
		PersonID: request.PersonID, CandidateMessageIDs: candidates, SourceClasses: lanes,
		SourceSince: request.Profile.SourceSince, SourceUntil: request.Profile.SourceUntil,
		HistoricalCandidateLimit: candidateLimit, Limit: candidateLimit,
		ThroughSequence: request.ThroughSequence,
	})
	if err != nil {
		return BriefWindow{}, fmt.Errorf("retrieve person brief evidence: %w", err)
	}

	eligible := make([]EvidenceItem, 0, len(retrieved))
	seen := make(map[string]struct{}, len(retrieved))
	appendEligible := func(item EvidenceItem) (bool, error) {
		if item.PersonID != request.PersonID {
			return false, fmt.Errorf("person brief evidence belongs to person %d, not %d",
				item.PersonID, request.PersonID)
		}
		// The profile may allow lanes the brief does not read, so the lane
		// check is against the window's lanes, not the profile's.
		if !sweepClassAllowed(lanes, item.SourceClass) {
			return false, nil
		}
		allowed, allowErr := assemblyEvidenceAllowed(item, request.Profile, after, before)
		if allowErr != nil || !allowed {
			return false, allowErr
		}
		id := packetEvidenceID(item)
		if _, duplicate := seen[id]; duplicate {
			return false, nil
		}
		seen[id] = struct{}{}
		eligible = append(eligible, item)
		return true, nil
	}
	for _, item := range retrieved {
		if _, err := appendEligible(item); err != nil {
			return BriefWindow{}, err
		}
	}

	lastContact, mandatory, err := briefLastContactItem(ctx, archive, request, lanes, after, before)
	if err != nil {
		return BriefWindow{}, err
	}
	if mandatory != nil {
		added, addErr := appendEligible(*mandatory)
		if addErr != nil {
			return BriefWindow{}, addErr
		}
		if !added {
			// Already retrieved; keep the copy that is in the window.
			mandatoryID := packetEvidenceID(*mandatory)
			for _, item := range eligible {
				if packetEvidenceID(item) == mandatoryID {
					copied := item
					mandatory = &copied
					break
				}
			}
		}
		lastContact.Included = true
		lastContact.EventTime = mandatory.EventTime.UTC()
	}
	sortBriefItemsNewestFirst(eligible)

	selected := briefSelectItems(eligible, mandatory, request)
	if len(selected) == 0 {
		return BriefWindow{}, ErrNoBriefEvidence
	}

	window := BriefWindow{Request: request, LastContact: lastContact}
	for {
		batch, boundary, buildErr := briefBatch(request, lanes, selected)
		if buildErr != nil {
			return BriefWindow{}, buildErr
		}
		if boundary.InputBytes <= request.MaxBytes {
			window.Batch = batch
			window.Boundary = boundary
			window.Items = selected
			return window, nil
		}
		trimmed, ok := briefDropOldest(selected, mandatory)
		if !ok {
			return BriefWindow{}, ErrEvidenceItemTooLarge
		}
		selected = trimmed
	}
}

// briefLastContactItem resolves person_contact_state's last-contact message.
// The design makes it mandatory so the brief and the deterministic timestamp
// agree on what "last time" is, but a missing, unscoped, or policy-excluded
// message never fails the window: the brief proceeds without it. The item must
// also sit in one of the window's lanes: contact state counts a meeting
// transcript as contact, but the brief does not read that lane yet.
func briefLastContactItem(
	ctx context.Context, archive BriefArchive, request BriefWindowRequest,
	lanes []SourceClass, after, before *time.Time,
) (BriefWindowLastContact, *EvidenceItem, error) {
	state, ok, err := archive.PersonSweepLastContact(ctx, request.PersonID)
	if err != nil {
		return BriefWindowLastContact{}, nil, fmt.Errorf("read person brief last contact: %w", err)
	}
	if !ok {
		return BriefWindowLastContact{}, nil, nil
	}
	result := BriefWindowLastContact{BriefLastContact: state}
	if state.MessageID <= 0 {
		return result, nil, nil
	}
	// Hydration reports a deleted, unscoped, or text-free message with
	// ErrPersonSweepMessageUnavailable, and so a last contact the journal
	// recorded after the window's sequence: the boundary must not claim a
	// message it does not cover. That is the one "message is missing" case the
	// window continues without. Any other failure is operational, a database
	// or provenance read that did not complete, and must fail the window
	// rather than quietly produce a brief without its last contact.
	items, err := archive.HydratePersonSweepMessages(
		ctx, request.PersonID, []int64{state.MessageID}, request.ThroughSequence)
	if err != nil {
		if ctx.Err() != nil {
			return BriefWindowLastContact{}, nil, ctx.Err()
		}
		if errors.Is(err, ErrPersonSweepMessageUnavailable) {
			return result, nil, nil
		}
		return BriefWindowLastContact{}, nil, fmt.Errorf("hydrate person brief last contact: %w", err)
	}
	for _, item := range items {
		if item.PersonID != request.PersonID || item.Ref.MessageID != state.MessageID ||
			!sweepClassAllowed(lanes, item.SourceClass) {
			continue
		}
		allowed, allowErr := assemblyEvidenceAllowed(item, request.Profile, after, before)
		if allowErr != nil {
			return BriefWindowLastContact{}, nil, allowErr
		}
		if !allowed {
			continue
		}
		candidate := item
		return result, &candidate, nil
	}
	return result, nil, nil
}

// briefSelectItems keeps the newest items within max_items, always keeping the
// mandatory last-contact item, and admits at most overlap_items that the
// previous brief already covered.
func briefSelectItems(
	items []EvidenceItem, mandatory *EvidenceItem, request BriefWindowRequest,
) []EvidenceItem {
	mandatoryID := ""
	if mandatory != nil {
		mandatoryID = packetEvidenceID(*mandatory)
	}
	selected := make([]EvidenceItem, 0, min(len(items), request.MaxItems))
	overlapUsed := 0
	for _, item := range items {
		if len(selected) == request.MaxItems {
			break
		}
		id := packetEvidenceID(item)
		if id == mandatoryID {
			selected = append(selected, item)
			continue
		}
		if request.Previous != nil && !item.EventTime.After(request.Previous.ThroughEventTime) {
			if overlapUsed == request.OverlapItems {
				continue
			}
			overlapUsed++
		}
		selected = append(selected, item)
	}
	if mandatoryID != "" && !slices.ContainsFunc(selected, func(item EvidenceItem) bool {
		return packetEvidenceID(item) == mandatoryID
	}) {
		if len(selected) == request.MaxItems {
			selected = selected[:len(selected)-1]
		}
		selected = append(selected, *mandatory)
		sortBriefItemsNewestFirst(selected)
	}
	return selected
}

// briefDropOldest removes the oldest item that is not the mandatory
// last-contact item. It reports false when nothing more can be dropped, which
// includes the case where one item alone already exceeds the byte cap.
func briefDropOldest(items []EvidenceItem, mandatory *EvidenceItem) ([]EvidenceItem, bool) {
	if len(items) < 2 {
		return nil, false
	}
	mandatoryID := ""
	if mandatory != nil {
		mandatoryID = packetEvidenceID(*mandatory)
	}
	for index := range slices.Backward(items) {
		if packetEvidenceID(items[index]) == mandatoryID {
			continue
		}
		return append(append([]EvidenceItem(nil), items[:index]...), items[index+1:]...), true
	}
	return nil, false
}

// briefBatch freezes the selected items into one canonical packet. Every item
// is a seed: the brief window has no changed-seed and retrieved-context split,
// and canonicalPacket treats both groups identically.
func briefBatch(
	request BriefWindowRequest, lanes []SourceClass, items []EvidenceItem,
) (PacketBatch, BriefBoundary, error) {
	packet, err := packetForProfile(EvidencePacket{
		PersonID: request.PersonID, ProgramID: BriefProgramID,
		ProgramVersion: BriefProgramVersion, Catalog: request.Catalog,
		Seeds: append([]EvidenceItem(nil), items...),
	}, request.Profile)
	if err != nil {
		return PacketBatch{}, BriefBoundary{}, fmt.Errorf("build person brief packet: %w", err)
	}
	packetJSON, err := marshalPacketEnvelope(packet)
	if err != nil {
		return PacketBatch{}, BriefBoundary{}, err
	}
	structured := briefStructuredRequest(packet, packetJSON, request.MaxOutputTokens)
	if _, err := validateStructuredRequest(structured, false); err != nil {
		return PacketBatch{}, BriefBoundary{}, fmt.Errorf("build person brief structured request: %w", err)
	}
	digest := sha256.Sum256(packetJSON)
	batch := PacketBatch{
		Ordinal: request.BatchOrdinal, InputHash: hex.EncodeToString(digest[:]),
		Request: structured, Packet: packet,
	}

	laneNames := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		laneNames = append(laneNames, string(lane))
	}
	from, through := items[0].EventTime, items[0].EventTime
	for _, item := range items {
		if item.EventTime.Before(from) {
			from = item.EventTime
		}
		if item.EventTime.After(through) {
			through = item.EventTime
		}
	}
	boundary := BriefBoundary{
		Lanes: laneNames, FromEventTime: from, ThroughEventTime: through,
		ThroughSequence: request.ThroughSequence, ItemCount: len(items),
		InputBytes: len(structured.InputText), PacketSHA256: batch.InputHash,
	}.Canonical()
	return batch, boundary, nil
}

func briefWindowLanes(allowed []SourceClass) []SourceClass {
	lanes := make([]SourceClass, 0, len(briefLanes))
	for _, lane := range briefLanes {
		if sweepClassAllowed(allowed, lane) {
			lanes = append(lanes, lane)
		}
	}
	return lanes
}

func sortBriefItemsNewestFirst(items []EvidenceItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].EventTime.Equal(items[j].EventTime) {
			return items[i].EventTime.After(items[j].EventTime)
		}
		return packetEvidenceID(items[i]) < packetEvidenceID(items[j])
	})
}
