package peoplesweep

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

type assemblySourceStub struct {
	windows        map[GenerationCursorMode]PersonWindow
	windowPages    map[GenerationCursorMode][]PersonWindow
	windowRequests []WindowRequest
	state          PersonFactState
	stateCatalog   personfacts.Catalog
	status         []personfacts.EvidenceStatusChange
}

func (s *assemblySourceStub) LoadPersonSweepWindow(_ context.Context, request WindowRequest) (PersonWindow, error) {
	s.windowRequests = append(s.windowRequests, request)
	if pages := s.windowPages[request.Mode]; len(pages) > 0 {
		window := pages[0]
		s.windowPages[request.Mode] = pages[1:]
		return window, nil
	}
	return s.windows[request.Mode], nil
}

func (s *assemblySourceStub) LoadPersonFactState(_ context.Context, _ int64, catalog personfacts.Catalog) (PersonFactState, error) {
	s.stateCatalog = catalog
	return s.state, nil
}

func (s *assemblySourceStub) BuildPersonSweepEvidenceStatusChanges(_ context.Context, _ int64, _ []ArchiveChange) ([]personfacts.EvidenceStatusChange, error) {
	return s.status, nil
}

type assemblyContextArchive struct {
	candidates []int64
	items      []EvidenceItem
	searches   []ContextRequest
}

func (a *assemblyContextArchive) ListPersonSweepHistoricalCandidates(context.Context, HistoricalCandidateRequest) ([]int64, error) {
	return append([]int64(nil), a.candidates...), nil
}

func (a *assemblyContextArchive) SearchPersonSweepMessages(_ context.Context, request ContextRequest) ([]EvidenceItem, error) {
	a.searches = append(a.searches, request)
	return append([]EvidenceItem(nil), a.items...), nil
}

func (a *assemblyContextArchive) HydratePersonSweepMessages(context.Context, int64, []int64) ([]EvidenceItem, error) {
	return []EvidenceItem{}, nil
}

func (a *assemblyContextArchive) SearchPersonSweepDocuments(context.Context, DocumentContextRequest) ([]EvidenceItem, error) {
	return []EvidenceItem{}, nil
}

func TestAssemblerUsesTargetDescriptionsForBoundedContext(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	seed := packet.Seeds[1]
	contextItem := packetTestEvidence(91, SourceConversationText, "bounded context")
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {
			Seeds: []EvidenceItem{seed}, NextSequence: 12,
			Changes: []ArchiveChange{{Sequence: 12, PersonID: 7, SourceLane: SourceConversationText}},
		},
	}, state: PersonFactState{Current: packet.CurrentProjection, Unresolved: packet.UnresolvedClaims}}
	archive := &assemblyContextArchive{candidates: []int64{91, 92}, items: []EvidenceItem{contextItem}}
	assembler := Assembler{
		Source: source, Context: NewContextRetriever(archive),
		MaxBytes: 16_384, MaxItems: 10, ContextPerTarget: 3,
	}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Cursors: []Cursor{{
			Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
				ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint},
			OptimisticSequence: 10, ReconciliationComplete: true,
			LastBackstopAt: new(time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)),
		}}, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText},
			SourceSince: "2020-01-01", AllowSensitive: true},
		Now: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC), BackstopInterval: 24 * time.Hour,
	})
	requirements.NoError(err)
	requirements.Len(result.Batches, 1)
	requirements.Len(archive.searches, len(packet.Catalog.Targets))
	for index, target := range packet.Catalog.Targets {
		checks.Equal(target, archive.searches[index].Target)
		checks.Equal([]int64{91, 92}, archive.searches[index].CandidateMessageIDs)
		checks.Equal(3, archive.searches[index].Limit)
	}
	checks.Len(result.Packet.Context, 1)
}

func TestAssemblerEmitsPartialDocumentCursorProgress(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	packet := packetTestPacket()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	seed := packetTestEvidence(10, SourceDocumentText, "document chunk")
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {
			Seeds: []EvidenceItem{seed}, NextSequence: 10, NextDocumentKey: "live:00000000000000000002",
			Changes: []ArchiveChange{{Sequence: 11, PersonID: 7, SourceLane: SourceDocumentText}},
		},
	}, state: PersonFactState{}}
	key := CursorKey{PersonID: 7, SourceLane: SourceDocumentText,
		ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint}
	result, err := (Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10,
		ContextPerTarget: 0}).Build(t.Context(), AssemblyRequest{
		PersonID: 7, Cursors: []Cursor{{Key: key, OptimisticSequence: 10,
			ReconciliationComplete: true, LastBackstopAt: &now}}, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceDocumentText},
			SourceSince: "2020-01-01", AllowSensitive: true},
		Now: now, BackstopInterval: 24 * time.Hour,
	})
	must.NoError(err)
	must.Len(result.CursorEnvelope, 1)
	checks.Equal(GenerationCursor{Key: key, Mode: GenerationCursorOptimistic,
		CursorFrom: 10, CursorThrough: 10, DocumentToKey: "live:00000000000000000002"},
		result.CursorEnvelope[0])
	must.Len(source.windowRequests, 1)
	checks.Empty(source.windowRequests[0].DocumentAfter)
}

func TestAssemblerEmitsPartialDocumentSourceKeyProgress(t *testing.T) {
	packet := packetTestPacket()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	seed := packetTestEvidence(10, SourceDocumentText, "document chunk")
	key := CursorKey{PersonID: 7, SourceLane: SourceDocumentText,
		ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint}
	tests := []struct {
		name      string
		mode      GenerationCursorMode
		cursor    Cursor
		window    PersonWindow
		force     bool
		wantUpper string
	}{
		{name: "reconciliation", mode: GenerationCursorReconciliation,
			cursor: Cursor{Key: key, ReconcileUpperKey: "00000000000000000010", LastBackstopAt: &now},
			window: PersonWindow{Seeds: []EvidenceItem{seed},
				NextDocumentKey: "live:00000000000000000002"}},
		{name: "backstop", mode: GenerationCursorBackstop,
			cursor: Cursor{Key: key, ReconciliationComplete: true}, force: true,
			window: PersonWindow{Seeds: []EvidenceItem{seed},
				NextDocumentKey:  "live:00000000000000000002",
				CapturedUpperKey: "00000000000000000010"},
			wantUpper: "00000000000000000010"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
				GenerationCursorOptimistic: {NextSequence: test.cursor.OptimisticSequence},
				test.mode:                  test.window,
			}}
			result, err := (Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10,
				ContextPerTarget: 0}).Build(t.Context(), AssemblyRequest{
				PersonID: 7, Cursors: []Cursor{test.cursor}, Catalog: packet.Catalog,
				Profile: ProviderProfile{AllowedSources: []SourceClass{SourceDocumentText},
					SourceSince: "2020-01-01", AllowSensitive: true},
				Now: now, BackstopInterval: 24 * time.Hour, ForceBackstop: test.force,
			})
			must.NoError(err)
			must.Len(result.CursorEnvelope, 1)
			checks.Equal(test.mode, result.CursorEnvelope[0].Mode)
			checks.Empty(result.CursorEnvelope[0].ReconcileFromKey)
			checks.Empty(result.CursorEnvelope[0].ReconcileToKey)
			checks.Empty(result.CursorEnvelope[0].DocumentFromKey)
			checks.Equal("live:00000000000000000002", result.CursorEnvelope[0].DocumentToKey)
			checks.Equal(test.wantUpper, result.CursorEnvelope[0].BackstopUpperKey)
		})
	}
}

func TestAssemblerBuildsPluralDueCursorEnvelope(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {
			Seeds: []EvidenceItem{packetTestEvidence(10, SourceConversationText, "optimistic seed")}, NextSequence: 12,
			Changes: []ArchiveChange{{Sequence: 12, PersonID: 7, SourceLane: SourceConversationText}},
		},
		GenerationCursorReconciliation: {
			Seeds: []EvidenceItem{packetTestEvidence(20, SourceConversationText, "reconciliation seed")}, NextReconcileKey: "00000000000000000020", ReconciliationDone: true,
		},
		GenerationCursorBackstop: {
			Seeds: []EvidenceItem{packetTestEvidence(30, SourceConversationText, "backstop seed")}, NextReconcileKey: "00000000000000000030",
			CapturedUpperKey: "00000000000000000030", ReconciliationDone: true,
		},
	}}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10, ContextPerTarget: 1}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{
			Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
				ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint},
			OptimisticSequence: 10, ReconcileAfterKey: "00000000000000000010",
			ReconcileUpperKey: "00000000000000000020", ReconciliationComplete: false,
			LastBackstopAt: new(now.Add(-25 * time.Hour)),
		}}, Now: now, BackstopInterval: 24 * time.Hour,
	})
	requirements.NoError(err)
	requirements.Len(result.CursorEnvelope, 3)
	checks.Equal([]GenerationCursorMode{
		GenerationCursorOptimistic, GenerationCursorReconciliation, GenerationCursorBackstop,
	}, []GenerationCursorMode{
		result.CursorEnvelope[0].Mode, result.CursorEnvelope[1].Mode, result.CursorEnvelope[2].Mode,
	})
	checks.Equal(int64(10), result.CursorEnvelope[0].CursorFrom)
	checks.Equal(int64(12), result.CursorEnvelope[0].CursorThrough)
	checks.Equal("00000000000000000010", result.CursorEnvelope[1].ReconcileFromKey)
	checks.Equal("00000000000000000020", result.CursorEnvelope[1].ReconcileToKey)
	checks.Empty(result.CursorEnvelope[2].ReconcileFromKey)
	checks.Equal("00000000000000000030", result.CursorEnvelope[2].ReconcileToKey)
	checks.Len(source.windowRequests, 3)
	checks.Equal([]GenerationCursorMode{
		GenerationCursorOptimistic, GenerationCursorReconciliation, GenerationCursorBackstop,
	}, []GenerationCursorMode{
		source.windowRequests[0].Mode, source.windowRequests[1].Mode, source.windowRequests[2].Mode,
	})
}

func TestAssemblerSkipsBackstopBeforeDue(t *testing.T) {
	packet := packetTestPacket()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{packet.Seeds[1]}, NextSequence: 2},
	}}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10}

	_, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{
			Key:                CursorKey{PersonID: 7, SourceLane: SourceConversationText, ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint},
			OptimisticSequence: 1, ReconciliationComplete: true, LastBackstopAt: new(now.Add(-time.Hour)),
		}}, Now: now, BackstopInterval: 24 * time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, source.windowRequests, 1)
	assert.Equal(t, GenerationCursorOptimistic, source.windowRequests[0].Mode)
}

func TestAssemblerPersistsOneBackstopPageWithinFreshCapturedUpper(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	source := &assemblySourceStub{
		windows: map[GenerationCursorMode]PersonWindow{
			GenerationCursorOptimistic: {Seeds: []EvidenceItem{packet.Seeds[1]}, NextSequence: 2},
		},
		windowPages: map[GenerationCursorMode][]PersonWindow{
			GenerationCursorBackstop: {
				{Seeds: []EvidenceItem{packetTestEvidence(20, SourceConversationText, "first page")},
					NextReconcileKey: "00000000000000000020", CapturedUpperKey: "00000000000000000030"},
				{Seeds: []EvidenceItem{packetTestEvidence(30, SourceConversationText, "second page")},
					NextReconcileKey: "00000000000000000030", CapturedUpperKey: "00000000000000000030", ReconciliationDone: true},
			},
		},
	}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 1}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint},
			OptimisticSequence: 1, ReconciliationComplete: true}},
		Now: now, BackstopInterval: 24 * time.Hour,
	})
	requirements.NoError(err)
	requirements.Len(source.windowRequests, 2)
	checks.Equal(WindowRequest{PersonID: 7, Lane: SourceConversationText,
		Mode: GenerationCursorBackstop, Limit: 1}, source.windowRequests[1])
	requirements.Len(result.CursorEnvelope, 2)
	checks.Equal(GenerationCursorBackstop, result.CursorEnvelope[1].Mode)
	checks.Empty(result.CursorEnvelope[1].ReconcileFromKey)
	checks.Equal("00000000000000000020", result.CursorEnvelope[1].ReconcileToKey)
	checks.Equal("00000000000000000030", result.CursorEnvelope[1].BackstopUpperKey)
	checks.Len(result.Packet.Seeds, 2)
}

func TestAssemblerResumesActiveBackstopEvenWhenNotDue(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {NextSequence: 1},
		GenerationCursorBackstop: {
			Seeds:            []EvidenceItem{packetTestEvidence(30, SourceConversationText, "resumed page")},
			NextReconcileKey: "00000000000000000030", CapturedUpperKey: "00000000000000000030",
			ReconciliationDone: true,
		},
	}}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 1}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint},
			OptimisticSequence: 1, ReconciliationComplete: true,
			BackstopUpperKey: "00000000000000000030", BackstopAfterKey: "00000000000000000020",
			LastBackstopAt: new(now.Add(-time.Hour))}},
		Now: now, BackstopInterval: 24 * time.Hour,
	})
	requirements.NoError(err)
	requirements.Len(source.windowRequests, 2)
	checks.Equal(WindowRequest{PersonID: 7, Lane: SourceConversationText,
		Mode: GenerationCursorBackstop, ReconcileAfter: "00000000000000000020",
		BackstopUpper: "00000000000000000030", Limit: 1}, source.windowRequests[1])
	requirements.Len(result.CursorEnvelope, 1)
	checks.Equal("00000000000000000020", result.CursorEnvelope[0].ReconcileFromKey)
	checks.Equal("00000000000000000030", result.CursorEnvelope[0].ReconcileToKey)
}

func TestAssemblerStatusOnlyWindowSkipsProviderBatches(t *testing.T) {
	checks := assert.New(t)
	packet := packetTestPacket()
	status := personfacts.EvidenceStatusChange{
		EvidenceKey: "evidence-key", SourceVersion: "source/v1",
		Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
	}
	source := &assemblySourceStub{
		windows: map[GenerationCursorMode]PersonWindow{
			GenerationCursorOptimistic: {
				NextSequence: 2,
				Changes: []ArchiveChange{{Sequence: 2, PersonID: 7, SourceLane: SourceConversationText,
					EvidenceEffect: EvidenceEffectSourceDeleted}},
			},
		},
		status: []personfacts.EvidenceStatusChange{status},
	}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint}, OptimisticSequence: 1, ReconciliationComplete: true,
			LastBackstopAt: new(time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC))}},
		Now: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC), BackstopInterval: 24 * time.Hour,
	})
	require.NoError(t, err)
	checks.Empty(result.Batches)
	checks.Equal([]personfacts.EvidenceStatusChange{status}, result.EvidenceStatusChanges)
	checks.Len(result.CursorEnvelope, 1)
}

func TestAssemblerExcludesEvidenceWithoutExactSubjectAttribution(t *testing.T) {
	packet := packetTestPacket()
	self := packetTestEvidence(10, SourceConversationText, "self-authored evidence")
	other := packetTestEvidence(11, SourceConversationText, "other-authored context")
	other.SubjectPersonID = nil
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {
			Seeds: []EvidenceItem{other, self}, NextSequence: 2,
			Changes: []ArchiveChange{{Sequence: 2, PersonID: 7, SourceLane: SourceConversationText}},
		},
	}}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint}, OptimisticSequence: 1, ReconciliationComplete: true,
			LastBackstopAt: new(time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC))}},
		Now: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC), BackstopInterval: 24 * time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, result.Packet.Seeds, 1)
	assert.Equal(t, self.Ref.MessageID, result.Packet.Seeds[0].Ref.MessageID)
}

func TestAssemblerRejectsWindowWithoutTextOrStatus(t *testing.T) {
	checks := assert.New(t)
	packet := packetTestPacket()
	lastBackstop := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	source := &assemblySourceStub{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic:     {NextSequence: 2},
		GenerationCursorReconciliation: {NextReconcileKey: "00000000000000000020"},
		GenerationCursorBackstop: {NextReconcileKey: "00000000000000000030",
			CapturedUpperKey: "00000000000000000030", ReconciliationDone: true},
	}}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10}

	result, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint},
			OptimisticSequence: 1, ReconcileAfterKey: "00000000000000000010",
			ReconcileUpperKey: "00000000000000000020", ReconciliationComplete: false,
			LastBackstopAt: &lastBackstop}},
		Now: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC), BackstopInterval: 24 * time.Hour,
	})
	require.ErrorIs(t, err, ErrNoChangedSeed)
	require.Len(t, result.CursorEnvelope, 3)
	checks.Equal(GenerationCursorOptimistic, result.CursorEnvelope[0].Mode)
	checks.Equal(int64(1), result.CursorEnvelope[0].CursorFrom)
	checks.Equal(int64(2), result.CursorEnvelope[0].CursorThrough)
	checks.Equal(GenerationCursorReconciliation, result.CursorEnvelope[1].Mode)
	checks.Equal("00000000000000000020", result.CursorEnvelope[1].ReconcileToKey)
	checks.Equal(GenerationCursorBackstop, result.CursorEnvelope[2].Mode)
	checks.Equal("00000000000000000030", result.CursorEnvelope[2].ReconcileToKey)
}

func TestAssemblerRejectsInvalidHostMetadataEvenWithStatusChanges(t *testing.T) {
	packet := packetTestPacket()
	invalid := packet.Seeds[1]
	invalid.Highlight.End--
	source := &assemblySourceStub{
		windows: map[GenerationCursorMode]PersonWindow{
			GenerationCursorOptimistic: {
				Seeds: []EvidenceItem{invalid}, NextSequence: 2,
				Changes: []ArchiveChange{{Sequence: 2, PersonID: 7, SourceLane: SourceConversationText}},
			},
		},
		status: []personfacts.EvidenceStatusChange{{
			EvidenceKey: "status", SourceVersion: "source/v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceEdited,
		}},
	}
	assembler := Assembler{Source: source, MaxBytes: 16_384, MaxItems: 10}

	_, err := assembler.Build(t.Context(), AssemblyRequest{
		PersonID: 7, Catalog: packet.Catalog,
		Profile: ProviderProfile{AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2020-01-01", AllowSensitive: true},
		Cursors: []Cursor{{Key: CursorKey{PersonID: 7, SourceLane: SourceConversationText,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: packet.Catalog.Fingerprint}, OptimisticSequence: 1, ReconciliationComplete: true,
			LastBackstopAt: new(time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC))}},
		Now: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC), BackstopInterval: 24 * time.Hour,
	})
	assert.ErrorContains(t, err, "host metadata")
}
