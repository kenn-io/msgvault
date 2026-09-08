package peoplesweep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
)

const briefTestPersonID int64 = 7

type briefFakeArchive struct {
	items           []EvidenceItem
	lastContact     BriefLastContact
	hasLastContact  bool
	hydrateFailures map[int64]error
	// journal is the sequence at which the person's change journal first saw
	// each message. A message absent from it has no journal row and is always
	// visible, as in the store.
	journal        map[int64]int64
	candidateCalls []HistoricalCandidateRequest
	searchCalls    []ContextRequest
	hydrateCalls   []int64
	documentCalls  int
}

// visibleAt mirrors the store's journal bound: a positive sequence hides a
// message the journal first recorded after it.
func (a *briefFakeArchive) visibleAt(messageID, throughSequence int64) bool {
	if throughSequence <= 0 {
		return true
	}
	sequence, journaled := a.journal[messageID]
	return !journaled || sequence <= throughSequence
}

func (a *briefFakeArchive) ListPersonSweepHistoricalCandidates(
	_ context.Context, request HistoricalCandidateRequest,
) ([]int64, error) {
	a.candidateCalls = append(a.candidateCalls, request)
	ids := make([]int64, 0, len(a.items))
	seen := make(map[int64]struct{}, len(a.items))
	for _, item := range a.newestFirst() {
		if !sweepClassAllowed(request.SourceClasses, item.SourceClass) ||
			!a.visibleAt(item.Ref.MessageID, request.ThroughSequence) {
			continue
		}
		// The store's AuthoredByPerson predicate admits exactly the messages
		// hydration marks with the person as their own subject.
		if request.AuthoredByPerson &&
			(item.SubjectPersonID == nil || *item.SubjectPersonID != request.PersonID) {
			continue
		}
		if _, ok := seen[item.Ref.MessageID]; ok {
			continue
		}
		seen[item.Ref.MessageID] = struct{}{}
		ids = append(ids, item.Ref.MessageID)
		if request.Limit > 0 && len(ids) == request.Limit {
			break
		}
	}
	return ids, nil
}

func (a *briefFakeArchive) SearchPersonSweepMessages(
	_ context.Context, request ContextRequest,
) ([]EvidenceItem, error) {
	a.searchCalls = append(a.searchCalls, request)
	// The store contract: an empty candidate list means "list your own
	// candidates", up to the store's evidence bound (2000, the same figure
	// as briefMaxCandidates) and without the authorship filter. Mirror it so
	// a window that leaks an empty list is caught.
	candidates := request.CandidateMessageIDs
	if len(candidates) == 0 {
		listed, err := a.ListPersonSweepHistoricalCandidates(context.Background(),
			HistoricalCandidateRequest{PersonID: request.PersonID,
				SourceClasses: request.SourceClasses, SourceSince: request.SourceSince,
				SourceUntil: request.SourceUntil, ThroughSequence: request.ThroughSequence,
				Limit: briefMaxCandidates})
		if err != nil {
			return nil, err
		}
		candidates = listed
	}
	allowed := make(map[int64]struct{}, len(candidates))
	for _, id := range candidates {
		allowed[id] = struct{}{}
	}
	out := make([]EvidenceItem, 0, len(a.items))
	for _, item := range a.newestFirst() {
		if _, ok := allowed[item.Ref.MessageID]; !ok {
			continue
		}
		if !sweepClassAllowed(request.SourceClasses, item.SourceClass) ||
			!a.visibleAt(item.Ref.MessageID, request.ThroughSequence) {
			continue
		}
		out = append(out, item)
		if request.Limit > 0 && len(out) == request.Limit {
			break
		}
	}
	return out, nil
}

func (a *briefFakeArchive) SearchPersonSweepDocuments(
	_ context.Context, _ DocumentContextRequest,
) ([]EvidenceItem, error) {
	a.documentCalls++
	return []EvidenceItem{}, nil
}

func (a *briefFakeArchive) HydratePersonSweepMessages(
	_ context.Context, _ int64, messageIDs []int64, throughSequence int64,
) ([]EvidenceItem, error) {
	a.hydrateCalls = append(a.hydrateCalls, throughSequence)
	out := make([]EvidenceItem, 0, len(messageIDs))
	for _, id := range messageIDs {
		if err, failed := a.hydrateFailures[id]; failed {
			return nil, err
		}
		found := false
		for _, item := range a.items {
			if item.Ref.MessageID == id && a.visibleAt(id, throughSequence) {
				out = append(out, item)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("message %d: %w", id, ErrPersonSweepMessageUnavailable)
		}
	}
	return out, nil
}

func (a *briefFakeArchive) PersonSweepLastContact(
	_ context.Context, _ int64,
) (BriefLastContact, bool, error) {
	return a.lastContact, a.hasLastContact, nil
}

func (a *briefFakeArchive) newestFirst() []EvidenceItem {
	items := append([]EvidenceItem(nil), a.items...)
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if items[j].EventTime.After(items[i].EventTime) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	return items
}

func briefWindowItem(messageID int64, lane SourceClass, day int, excerpt string) EvidenceItem {
	person := briefTestPersonID
	ref := EvidenceRef{
		SourceLane: lane, SourceID: 2, MessageID: messageID,
		SourceMessageID: "source-message", SpanEnd: len([]rune(excerpt)),
	}
	hash := sha256.Sum256([]byte(excerpt))
	directness := personfacts.DirectSelf
	if lane == SourceMeetingText {
		directness = personfacts.DirectOther
	}
	return EvidenceItem{
		Ref: ref, PersonID: person, SubjectPersonID: &person, SourceClass: lane,
		SourceVersion: "source/v1", ContentSHA256: hex.EncodeToString(hash[:]),
		EventTime:    time.Date(2026, 8, day, 12, 0, 0, 0, time.UTC),
		RecordedTime: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Excerpt:      excerpt, Highlight: TextSpan{End: len([]rune(excerpt))},
		Provenance: personscope.Provenance{
			ParticipantIDs: []int64{8, 9}, Roles: []personscope.Role{personscope.RoleFrom},
			Directions: []personscope.Direction{personscope.FromPerson},
		},
		IdentityBasisPoints: 950, Directness: directness,
		Authority: personfacts.AuthorityOrdinary,
	}
}

func briefWindowCatalog(t *testing.T) personfacts.Catalog {
	t.Helper()
	targets := []personfacts.TargetDescriptor{
		packetTestTarget("target:food", "favorite food"),
		packetTestTarget("target:role", "employment role"),
	}
	targets[1].Sensitive = true
	revision, err := personfacts.DescriptorRevision(targets[1])
	require.NoError(t, err)
	targets[1].Revision = revision
	fingerprint, err := personfacts.CatalogFingerprint(targets)
	require.NoError(t, err)
	return personfacts.Catalog{Version: "1", Fingerprint: fingerprint, Targets: targets}
}

func briefWindowProfile() ProviderProfile {
	return ProviderProfile{
		Fingerprint:    "brief-profile",
		AllowedSources: []SourceClass{SourceConversationText, SourceMeetingText},
		SourceSince:    "2026-01-01", AllowSensitive: true,
	}
}

func briefWindowRequest(t *testing.T) BriefWindowRequest {
	t.Helper()
	return BriefWindowRequest{
		PersonID: briefTestPersonID, Catalog: briefWindowCatalog(t), Profile: briefWindowProfile(),
		MaxItems: 4, MaxBytes: 65536, OverlapItems: 2, ThroughSequence: 4200,
	}
}

func TestBuildBriefWindowTakesNewestItemsWithinCaps(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "oldest note"),
		briefWindowItem(11, SourceConversationText, 12, "older note"),
		briefWindowItem(12, SourceConversationText, 14, "middle note"),
		briefWindowItem(13, SourceConversationText, 16, "newer note"),
		briefWindowItem(14, SourceConversationText, 18, "newest note"),
	}}

	require := require.New(t)
	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)

	require.Len(window.Items, 4)
	assert.Equal([]int64{14, 13, 12, 11}, briefMessageIDs(window.Items),
		"the window is newest-first and stops at max_items")
	assert.Equal(4, window.Boundary.ItemCount)
	assert.Equal(int64(4200), window.Boundary.ThroughSequence)
	assert.Equal(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC), window.Boundary.FromEventTime)
	assert.Equal(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), window.Boundary.ThroughEventTime)
	assert.Equal([]string{"conversation_text"}, window.Boundary.Lanes)
	assert.Equal(len(window.Batch.Request.InputText), window.Boundary.InputBytes)
	assert.Equal(window.Batch.InputHash, window.Boundary.PacketSHA256)
	assert.Zero(archive.documentCalls, "the document lane has no event-time listing path")
}

// TestBuildBriefWindowReachesPersonMessagesBehindOwnerHeavyTraffic pins the
// listing's eligibility filter. The window admits only evidence the person is
// the subject of, so the candidate listing must apply the same rule: a
// conversation where the owner wrote the newest max_items * headroom messages
// used to exhaust the candidate cap before any of the person's own messages,
// and the brief reported no evidence for a person with plenty.
func TestBuildBriefWindowReachesPersonMessagesBehindOwnerHeavyTraffic(t *testing.T) {
	ownerAuthored := func(messageID int64, day int, excerpt string) EvidenceItem {
		item := briefWindowItem(messageID, SourceConversationText, day, excerpt)
		item.SubjectPersonID = nil
		item.Directness = personfacts.DirectOther
		item.Provenance.Roles = []personscope.Role{personscope.RoleTo}
		item.Provenance.Directions = []personscope.Direction{personscope.ToPerson}
		return item
	}
	items := []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 2, "the person's own older message"),
		briefWindowItem(11, SourceConversationText, 3, "the person's own newer message"),
	}
	for day := 4; day < 4+briefCandidateHeadroom*2; day++ {
		items = append(items, ownerAuthored(int64(100+day), day, "the owner writing again"))
	}
	archive := &briefFakeArchive{items: items}
	request := briefWindowRequest(t)
	request.MaxItems = 2

	require := require.New(t)
	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	assert.Equal([]int64{11, 10}, briefMessageIDs(window.Items),
		"the person's messages are reached past the owner's newer traffic")
	require.Len(archive.candidateCalls, 1)
	assert.True(archive.candidateCalls[0].AuthoredByPerson,
		"the listing filters authorship instead of leaving it to the window")
	assert.Equal(request.MaxItems*briefCandidateHeadroom, archive.candidateCalls[0].Limit)
}

// TestBuildBriefWindowStopsWhenNoCandidateIsPersonAuthored pins the guard in
// front of retrieval. The search treats an empty candidate list as "list your
// own", without the authorship filter, so a contact who authored nothing on an
// authenticating source used to have their whole scope hydrated and rejected;
// the window must answer no evidence without a search at all.
func TestBuildBriefWindowStopsWhenNoCandidateIsPersonAuthored(t *testing.T) {
	ownerOnly := briefWindowItem(10, SourceConversationText, 10, "the owner wrote this")
	ownerOnly.SubjectPersonID = nil
	ownerOnly.Directness = personfacts.DirectOther
	archive := &briefFakeArchive{items: []EvidenceItem{ownerOnly}}
	assert := assert.New(t)

	_, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.ErrorIs(t, err, ErrNoBriefEvidence)
	assert.Len(archive.candidateCalls, 1)
	assert.Empty(archive.searchCalls, "no search runs on an empty candidate list")
	assert.Empty(archive.hydrateCalls)
}

func TestBuildBriefWindowBindsTheCanonicalBriefPacket(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "oldest note"),
		briefWindowItem(11, SourceConversationText, 12, "second note"),
	}}

	require := require.New(t)
	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)

	assert.Equal(BriefProgramID, window.Batch.Packet.ProgramID)
	assert.Equal(BriefProgramVersion, window.Batch.Packet.ProgramVersion)
	assert.Equal(briefTestPersonID, window.Batch.Packet.PersonID)
	assert.Empty(window.Batch.Packet.Context, "the brief window is one packet of seeds")
	assert.Len(window.Batch.Packet.Seeds, 2)
	assert.True(window.Batch.Request.ContainsSensitive)

	packetJSON, err := marshalPacketEnvelope(window.Batch.Packet)
	require.NoError(err)
	digest := sha256.Sum256(packetJSON)
	assert.Equal(hex.EncodeToString(digest[:]), window.Batch.InputHash)
	assert.Equal(briefStructuredRequest(window.Batch.Packet, packetJSON,
		window.Request.MaxOutputTokens), window.Batch.Request)

	// The packet round-trips through the same canonical path extraction uses.
	canonical, err := canonicalPacket(window.Batch.Packet)
	require.NoError(err)
	assert.Equal(window.Batch.Packet, canonical)
}

func TestBuildBriefWindowFiltersDisallowedLanesDatesAndSensitiveTargets(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 12, "in window"),
		briefWindowItem(11, SourceConversationText, 9, "before source_since"),
		briefWindowItem(12, SourceMeetingText, 14, "meeting outside the brief lanes"),
	}}
	request := briefWindowRequest(t)
	request.Profile.SourceSince = "2026-08-11"
	request.Profile.AllowSensitive = false

	require := require.New(t)
	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)

	assert.Equal([]int64{10}, briefMessageIDs(window.Items))
	assert.Equal([]string{"conversation_text"}, window.Boundary.Lanes)
	require.Len(window.Batch.Packet.Catalog.Targets, 1)
	assert.Equal("target:food", window.Batch.Packet.Catalog.Targets[0].Key,
		"a sensitive target is excluded from a profile that does not allow it")
	assert.True(window.Batch.Request.ContainsSensitive,
		"archive excerpts keep the packet sensitive regardless of catalog filtering")
}

func TestBuildBriefWindowExcludesItemsBeforeTheProfileWindow(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 12, "before the window"),
		briefWindowItem(11, SourceConversationText, 20, "inside the window"),
	}}
	request := briefWindowRequest(t)
	request.Profile.SourceSince = "2026-08-15"

	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err)
	assert.Equal(t, []int64{11}, briefMessageIDs(window.Items))
}

func TestBuildBriefWindowAlwaysIncludesTheLastContactItem(t *testing.T) {
	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(10, SourceConversationText, 10, "the deterministic last contact"),
			briefWindowItem(11, SourceConversationText, 12, "later one"),
			briefWindowItem(12, SourceConversationText, 14, "later two"),
			briefWindowItem(13, SourceConversationText, 16, "later three"),
			briefWindowItem(14, SourceConversationText, 18, "later four"),
		},
		lastContact:    BriefLastContact{MessageID: 10, SourceID: 2, Channel: "chat"},
		hasLastContact: true,
	}

	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(t, err)

	assert.Contains(briefMessageIDs(window.Items), int64(10),
		"the brief and the deterministic last-contact timestamp must agree")
	assert.Len(window.Items, 4)
	assert.True(window.LastContact.Included)
	assert.Equal("chat", window.LastContact.Channel)
	assert.Equal(time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC), window.LastContact.EventTime)
}

func TestBuildBriefWindowExcludesLastContactWithoutPersonAuthorship(t *testing.T) {
	for _, kind := range []string{"owner-authored", "unauthenticated-sender", "different-subject"} {
		t.Run(kind, func(t *testing.T) {
			last := briefWindowItem(12, SourceConversationText, 14, "the latest message")
			last.SubjectPersonID = nil
			last.Directness = personfacts.DirectOther
			if kind == "owner-authored" {
				last.Provenance.Roles = []personscope.Role{personscope.RoleTo}
				last.Provenance.Directions = []personscope.Direction{personscope.ToPerson}
			}
			if kind == "different-subject" {
				last.SubjectPersonID = new(briefTestPersonID + 1)
			}
			archive := &briefFakeArchive{
				items: []EvidenceItem{
					briefWindowItem(11, SourceConversationText, 12, "the person shared an update"),
					last,
				},
				lastContact:    BriefLastContact{MessageID: 12, SourceID: 2, Channel: "chat"},
				hasLastContact: true,
			}
			window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
			require.NoError(t, err)
			assert.Equal(t, []int64{11}, briefMessageIDs(window.Items))
			assert.False(t, window.LastContact.Included)
		})
	}
}

// TestBuildBriefWindowBoundsEveryReadByTheCapturedSequence pins the boundary
// contract: the window records ThroughSequence as what it saw, and the next
// run regenerates only on journal rows past it. A message journaled after the
// capture must therefore stay out of the window on every path, including the
// deterministic last contact, or two briefs with the same boundary could hold
// different evidence.
func TestBuildBriefWindowBoundsEveryReadByTheCapturedSequence(t *testing.T) {
	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(10, SourceConversationText, 10, "before the journal existed"),
			briefWindowItem(11, SourceConversationText, 12, "at the bound"),
			briefWindowItem(12, SourceConversationText, 14, "committed after planning"),
			briefWindowItem(13, SourceConversationText, 16, "the last contact, also after planning"),
		},
		journal:        map[int64]int64{11: 4200, 12: 4201, 13: 4202},
		lastContact:    BriefLastContact{MessageID: 13, SourceID: 2, Channel: "chat"},
		hasLastContact: true,
	}

	require := require.New(t)
	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)

	assert.Equal([]int64{11, 10}, briefMessageIDs(window.Items),
		"the item at the bound and the unjournaled item are in; later commits are out")
	assert.Equal(int64(4200), window.Boundary.ThroughSequence)
	assert.False(window.LastContact.Included,
		"a last contact the journal saw after the bound is treated as missing")
	assert.Equal("chat", window.LastContact.Channel)
	require.Len(archive.candidateCalls, 1)
	assert.Equal(int64(4200), archive.candidateCalls[0].ThroughSequence)
	require.Len(archive.searchCalls, 1)
	assert.Equal(int64(4200), archive.searchCalls[0].ThroughSequence)
	assert.Equal([]int64{4200}, archive.hydrateCalls,
		"last-contact hydration carries the same bound")
}

func TestBuildBriefWindowProceedsWhenTheLastContactMessageIsMissing(t *testing.T) {
	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(11, SourceConversationText, 12, "still here"),
		},
		lastContact:     BriefLastContact{MessageID: 99, SourceID: 2, Channel: "email"},
		hasLastContact:  true,
		hydrateFailures: map[int64]error{99: fmt.Errorf("message 99: %w", ErrPersonSweepMessageUnavailable)},
	}

	assert := assert.New(t)

	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(t, err)
	assert.Equal([]int64{11}, briefMessageIDs(window.Items))
	assert.False(window.LastContact.Included)
	assert.Equal("email", window.LastContact.Channel)
}

// TestBuildBriefWindowFailsWhenLastContactHydrationBreaks separates the one
// tolerated hydration outcome, a message that is genuinely unavailable, from
// an operational failure. A database or provenance read that did not complete
// says nothing about whether the message exists, so the window must not
// proceed as if the last contact were missing and store a brief without it.
func TestBuildBriefWindowFailsWhenLastContactHydrationBreaks(t *testing.T) {
	broken := errors.New("read person message provenance: database is locked")
	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(11, SourceConversationText, 12, "still here"),
		},
		lastContact:     BriefLastContact{MessageID: 99, SourceID: 2, Channel: "email"},
		hasLastContact:  true,
		hydrateFailures: map[int64]error{99: broken},
	}

	_, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.ErrorIs(t, err, broken)
	assert.NotErrorIs(t, err, ErrNoBriefEvidence)
}

func TestBuildBriefWindowCapsOverlapWithThePreviousBoundary(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "covered three"),
		briefWindowItem(11, SourceConversationText, 12, "covered two"),
		briefWindowItem(12, SourceConversationText, 14, "covered one"),
		briefWindowItem(13, SourceConversationText, 16, "new one"),
	}}
	request := briefWindowRequest(t)
	request.MaxItems = 10
	request.OverlapItems = 2
	request.Previous = &BriefBoundary{
		ThroughEventTime: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}

	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err)
	assert.Equal(t, []int64{13, 12, 11}, briefMessageIDs(window.Items),
		"one new item plus at most overlap_items already-covered items")
}

func TestBuildBriefWindowTrimsOldestItemsToTheByteCap(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "oldest note"),
		briefWindowItem(11, SourceConversationText, 12, "middle note"),
		briefWindowItem(12, SourceConversationText, 14, "newest note"),
	}}
	require := require.New(t)
	assert := assert.New(t)

	full, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)
	require.Len(full.Items, 3)

	request := briefWindowRequest(t)
	request.MaxBytes = full.Boundary.InputBytes - 1
	trimmed, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	assert.Equal([]int64{12, 11}, briefMessageIDs(trimmed.Items))
	assert.LessOrEqual(trimmed.Boundary.InputBytes, request.MaxBytes)
}

func TestBuildBriefWindowRefusesAWindowThatCannotFitOneItem(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "one note"),
		briefWindowItem(11, SourceConversationText, 12, "another note"),
	}}
	request := briefWindowRequest(t)
	request.MaxBytes = 1
	_, err := BuildBriefWindow(t.Context(), archive, request)
	require.ErrorIs(t, err, ErrEvidenceItemTooLarge)
}

func TestBuildBriefWindowRejectsAnEmptyWindow(t *testing.T) {
	archive := &briefFakeArchive{}
	_, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.ErrorIs(t, err, ErrNoBriefEvidence)
}

func TestBuildBriefWindowRejectsInvalidRequests(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "note"),
	}}
	for name, mutate := range map[string]func(*BriefWindowRequest){
		"no person":     func(r *BriefWindowRequest) { r.PersonID = 0 },
		"no items":      func(r *BriefWindowRequest) { r.MaxItems = 0 },
		"no bytes":      func(r *BriefWindowRequest) { r.MaxBytes = 0 },
		"bad overlap":   func(r *BriefWindowRequest) { r.OverlapItems = -1 },
		"bad sequence":  func(r *BriefWindowRequest) { r.ThroughSequence = -1 },
		"no brief lane": func(r *BriefWindowRequest) { r.Profile.AllowedSources = []SourceClass{SourceDocumentText} },
	} {
		t.Run(name, func(t *testing.T) {
			request := briefWindowRequest(t)
			mutate(&request)
			_, err := BuildBriefWindow(t.Context(), archive, request)
			require.Error(t, err)
		})
	}
}

// TestBuildBriefWindowExcludesMeetingTranscriptsUntilSpeakerAttribution pins
// ruling R13: the v1 brief window reads conversation_text only. Meeting
// importers do not authenticate who said what, so a meeting utterance can never
// be evidence the person is the subject of, and the lane stays out of the
// window even when the profile allows it: it is not listed, not searched, not
// admitted as the deterministic last contact, and a meeting-only history
// produces no brief at all.
func TestBuildBriefWindowExcludesMeetingTranscriptsUntilSpeakerAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(10, SourceConversationText, 10, "a chat message"),
			briefWindowItem(11, SourceMeetingText, 12, "the person spoke in a meeting"),
			briefWindowItem(12, SourceMeetingText, 14, "the person spoke in the last meeting"),
		},
		lastContact:    BriefLastContact{MessageID: 12, SourceID: 2, Channel: "meeting"},
		hasLastContact: true,
	}
	request := briefWindowRequest(t)
	request.Profile.AllowedSources = []SourceClass{SourceConversationText, SourceMeetingText}

	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)

	assert.Equal([]int64{10}, briefMessageIDs(window.Items), "meeting items never enter the window")
	assert.Equal([]string{"conversation_text"}, window.Boundary.Lanes)
	require.Len(archive.candidateCalls, 1)
	assert.Equal([]SourceClass{SourceConversationText}, archive.candidateCalls[0].SourceClasses,
		"the listing is never asked for the meeting lane")
	require.Len(archive.searchCalls, 1)
	assert.Equal([]SourceClass{SourceConversationText}, archive.searchCalls[0].SourceClasses)
	assert.False(window.LastContact.Included,
		"a meeting last contact is reported but not admitted into the window")
	assert.Equal(int64(12), window.LastContact.MessageID)
	for _, item := range window.Batch.Packet.Seeds {
		assert.Equal(SourceConversationText, item.SourceClass)
	}

	meetingOnly := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(11, SourceMeetingText, 12, "the person spoke in a meeting"),
	}}
	_, err = BuildBriefWindow(t.Context(), meetingOnly, request)
	require.ErrorIs(err, ErrNoBriefEvidence, "a meeting-only history has no brief evidence")

	request.Profile.AllowedSources = []SourceClass{SourceMeetingText}
	_, err = BuildBriefWindow(t.Context(), archive, request)
	require.Error(err)
	assert.Contains(err.Error(), "no allowed source lane",
		"a meeting-only profile has no supported brief lane")
	assert.Empty(briefWindowLanes([]SourceClass{SourceMeetingText, SourceDocumentText}))
}

func TestBriefBoundaryJSONIsTheFrozenShape(t *testing.T) {
	boundary := BriefBoundary{
		Lanes:            []string{"meeting_text", "conversation_text"},
		FromEventTime:    time.Date(2026, 8, 12, 12, 0, 0, 500_000_000, time.UTC),
		ThroughEventTime: time.Date(2026, 8, 29, 9, 30, 0, 0, time.FixedZone("CEST", 2*3600)),
		ThroughSequence:  4200, ItemCount: 6, InputBytes: 8192,
		PacketSHA256: "a1b2",
	}
	require := require.New(t)
	assert := assert.New(t)

	encoded, err := json.Marshal(boundary.Canonical())
	require.NoError(err)
	assert.JSONEq(
		`{"lanes":["conversation_text","meeting_text"],"from_event_time":"2026-08-12T12:00:00Z",`+
			`"through_event_time":"2026-08-29T07:30:00Z","through_sequence":4200,"item_count":6,`+
			`"input_bytes":8192,"packet_sha256":"a1b2"}`,
		string(encoded))
	assert.Equal(
		[]string{"lanes", "from_event_time", "through_event_time", "through_sequence",
			"item_count", "input_bytes", "packet_sha256"},
		jsonKeyOrder(t, encoded), "boundary_json key order is part of the stored record")

	var decoded BriefBoundary
	require.NoError(json.Unmarshal(encoded, &decoded))
	assert.Equal(boundary.Canonical(), decoded)
}

func jsonKeyOrder(t *testing.T, encoded []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), token)
	keys := make([]string, 0, 8)
	for decoder.More() {
		key, keyErr := decoder.Token()
		require.NoError(t, keyErr)
		name, ok := key.(string)
		require.True(t, ok)
		keys = append(keys, name)
		var value json.RawMessage
		require.NoError(t, decoder.Decode(&value))
	}
	return keys
}

func briefMessageIDs(items []EvidenceItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Ref.MessageID)
	}
	return ids
}
