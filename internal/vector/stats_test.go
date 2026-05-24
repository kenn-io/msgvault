package vector

import (
	"context"
	"errors"
	"testing"
	"time"
)

// statsFakeBackend implements Backend for CollectStats tests. It is
// separate from the generations_test.go fakeBackend because those tests
// stub different methods and we want independent control. Methods not
// exercised here return errors so any misuse surfaces loudly.
type statsFakeBackend struct {
	active     *Generation
	activeErr  error
	building   *Generation
	buildErr   error
	statsByGen map[GenerationID]Stats
	statsErr   map[GenerationID]error
}

func (f *statsFakeBackend) ActiveGeneration(context.Context) (Generation, error) {
	if f.activeErr != nil {
		return Generation{}, f.activeErr
	}
	if f.active == nil {
		return Generation{}, ErrNoActiveGeneration
	}
	return *f.active, nil
}

func (f *statsFakeBackend) BuildingGeneration(context.Context) (*Generation, error) {
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return f.building, nil
}

func (f *statsFakeBackend) Stats(_ context.Context, gen GenerationID) (Stats, error) {
	if err, ok := f.statsErr[gen]; ok {
		return Stats{}, err
	}
	return f.statsByGen[gen], nil
}

func (f *statsFakeBackend) CreateGeneration(context.Context, string, int, string) (GenerationID, error) {
	return 0, errors.New("not implemented")
}

func (f *statsFakeBackend) ActivateGeneration(context.Context, GenerationID) error {
	return errors.New("not implemented")
}

func (f *statsFakeBackend) RetireGeneration(context.Context, GenerationID) error {
	return errors.New("not implemented")
}

func (f *statsFakeBackend) Upsert(context.Context, GenerationID, []Chunk) error {
	return errors.New("not implemented")
}

func (f *statsFakeBackend) Search(context.Context, GenerationID, []float32, int, Filter) ([]Hit, error) {
	return nil, errors.New("not implemented")
}

func (f *statsFakeBackend) Delete(context.Context, GenerationID, []int64) error {
	return errors.New("not implemented")
}

func (f *statsFakeBackend) LoadVector(context.Context, int64) ([]float32, error) {
	return nil, errors.New("not implemented")
}

func (f *statsFakeBackend) Close() error { return nil }

func (f *statsFakeBackend) EnsureSeeded(context.Context, GenerationID) error {
	return errors.New("not implemented")
}

var _ Backend = (*statsFakeBackend)(nil)

func TestCollectStats_NilBackend(t *testing.T) {
	sv, err := CollectStats(context.Background(), nil)
	if err != nil {
		t.Fatalf("CollectStats(nil) err = %v, want nil", err)
	}
	if sv != nil {
		t.Errorf("CollectStats(nil) = %+v, want nil", sv)
	}
}

func TestCollectStats_ActiveOnly(t *testing.T) {
	activatedAt := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	b := &statsFakeBackend{
		active: &Generation{
			ID:          5,
			Model:       "nomic-embed",
			Dimension:   768,
			Fingerprint: "nomic-embed:768",
			State:       GenerationActive,
			ActivatedAt: &activatedAt,
		},
		statsByGen: map[GenerationID]Stats{
			5: {EmbeddingCount: 100, PendingCount: 7},
		},
	}

	sv, err := CollectStats(context.Background(), b)
	if err != nil {
		t.Fatalf("CollectStats err = %v", err)
	}
	if sv == nil {
		t.Fatal("CollectStats returned nil StatsView")
	}
	if !sv.Enabled {
		t.Error("Enabled = false, want true")
	}
	if sv.ActiveGeneration == nil {
		t.Fatal("ActiveGeneration is nil, want populated")
	}
	ag := sv.ActiveGeneration
	if ag.ID != 5 {
		t.Errorf("ActiveGeneration.ID = %d, want 5", ag.ID)
	}
	if ag.Model != "nomic-embed" {
		t.Errorf("ActiveGeneration.Model = %q, want 'nomic-embed'", ag.Model)
	}
	if ag.Dimension != 768 {
		t.Errorf("ActiveGeneration.Dimension = %d, want 768", ag.Dimension)
	}
	if ag.Fingerprint != "nomic-embed:768" {
		t.Errorf("ActiveGeneration.Fingerprint = %q, want 'nomic-embed:768'", ag.Fingerprint)
	}
	if ag.State != "active" {
		t.Errorf("ActiveGeneration.State = %q, want 'active'", ag.State)
	}
	if ag.MessageCount != 100 {
		t.Errorf("ActiveGeneration.MessageCount = %d, want 100", ag.MessageCount)
	}
	if ag.ActivatedAt != activatedAt.Format(time.RFC3339) {
		t.Errorf("ActiveGeneration.ActivatedAt = %q, want %q",
			ag.ActivatedAt, activatedAt.Format(time.RFC3339))
	}
	if sv.BuildingGeneration != nil {
		t.Errorf("BuildingGeneration = %+v, want nil", sv.BuildingGeneration)
	}
	if sv.PendingEmbeddingsTotal != 7 {
		t.Errorf("PendingEmbeddingsTotal = %d, want 7", sv.PendingEmbeddingsTotal)
	}
}

func TestCollectStats_BuildingOnly(t *testing.T) {
	startedAt := time.Date(2025, 4, 15, 9, 30, 0, 0, time.UTC)
	b := &statsFakeBackend{
		// No active generation — first build scenario.
		building: &Generation{
			ID:        9,
			Model:     "nomic-embed",
			Dimension: 768,
			StartedAt: startedAt,
		},
		statsByGen: map[GenerationID]Stats{
			9: {EmbeddingCount: 40, PendingCount: 60},
		},
	}

	sv, err := CollectStats(context.Background(), b)
	if err != nil {
		t.Fatalf("CollectStats err = %v", err)
	}
	if sv == nil {
		t.Fatal("CollectStats returned nil StatsView")
	}
	if !sv.Enabled {
		t.Error("Enabled = false, want true")
	}
	if sv.ActiveGeneration != nil {
		t.Errorf("ActiveGeneration = %+v, want nil", sv.ActiveGeneration)
	}
	if sv.BuildingGeneration == nil {
		t.Fatal("BuildingGeneration is nil, want populated")
	}
	bg := sv.BuildingGeneration
	if bg.ID != 9 {
		t.Errorf("BuildingGeneration.ID = %d, want 9", bg.ID)
	}
	if bg.Model != "nomic-embed" {
		t.Errorf("BuildingGeneration.Model = %q, want 'nomic-embed'", bg.Model)
	}
	if bg.Dimension != 768 {
		t.Errorf("BuildingGeneration.Dimension = %d, want 768", bg.Dimension)
	}
	if bg.StartedAt != startedAt.Format(time.RFC3339) {
		t.Errorf("BuildingGeneration.StartedAt = %q, want %q",
			bg.StartedAt, startedAt.Format(time.RFC3339))
	}
	if bg.Progress.Done != 40 {
		t.Errorf("Progress.Done = %d, want 40", bg.Progress.Done)
	}
	if bg.Progress.Total != 100 {
		t.Errorf("Progress.Total = %d, want 100", bg.Progress.Total)
	}
	if sv.PendingEmbeddingsTotal != 60 {
		t.Errorf("PendingEmbeddingsTotal = %d, want 60", sv.PendingEmbeddingsTotal)
	}
}

func TestCollectStats_BothGenerations(t *testing.T) {
	activatedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	b := &statsFakeBackend{
		active: &Generation{
			ID:          1,
			Model:       "m1",
			Dimension:   384,
			Fingerprint: "m1:384",
			State:       GenerationActive,
			ActivatedAt: &activatedAt,
		},
		building: &Generation{
			ID:        2,
			Model:     "m2",
			Dimension: 768,
			StartedAt: startedAt,
		},
		statsByGen: map[GenerationID]Stats{
			1: {EmbeddingCount: 500, PendingCount: 3},
			2: {EmbeddingCount: 50, PendingCount: 450},
		},
	}

	sv, err := CollectStats(context.Background(), b)
	if err != nil {
		t.Fatalf("CollectStats err = %v", err)
	}
	if sv == nil {
		t.Fatal("CollectStats returned nil StatsView")
	}
	if sv.ActiveGeneration == nil || sv.ActiveGeneration.ID != 1 {
		t.Errorf("ActiveGeneration = %+v, want ID=1", sv.ActiveGeneration)
	}
	if sv.BuildingGeneration == nil || sv.BuildingGeneration.ID != 2 {
		t.Errorf("BuildingGeneration = %+v, want ID=2", sv.BuildingGeneration)
	}
	// Sum of both pending counts: 3 + 450.
	if sv.PendingEmbeddingsTotal != 453 {
		t.Errorf("PendingEmbeddingsTotal = %d, want 453", sv.PendingEmbeddingsTotal)
	}
}

func TestCollectStats_ActiveError(t *testing.T) {
	// A non-sentinel error from ActiveGeneration is joined into the
	// returned error, but the envelope (Enabled=true) is still returned
	// so callers can log the failure and still render whatever partial
	// stats came back (e.g. a building generation).
	wantErr := errors.New("db connection refused")
	b := &statsFakeBackend{activeErr: wantErr}

	sv, err := CollectStats(context.Background(), b)
	if err == nil {
		t.Fatalf("CollectStats err = nil, want wrapping %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("CollectStats err = %v, want to wrap %v", err, wantErr)
	}
	if sv == nil {
		t.Fatal("CollectStats sv = nil, want non-nil envelope even on partial failure")
	}
	if !sv.Enabled {
		t.Error("Enabled = false, want true (backend is non-nil)")
	}
	if sv.ActiveGeneration != nil {
		t.Errorf("ActiveGeneration = %+v, want nil (lookup failed)", sv.ActiveGeneration)
	}
}

func TestCollectStats_BuildingError(t *testing.T) {
	// A non-sentinel error from BuildingGeneration is joined into the
	// returned error, symmetric with the ActiveGeneration error path.
	wantErr := errors.New("db connection refused")
	b := &statsFakeBackend{buildErr: wantErr}

	sv, err := CollectStats(context.Background(), b)
	if err == nil {
		t.Fatalf("CollectStats err = nil, want wrapping %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("CollectStats err = %v, want to wrap %v", err, wantErr)
	}
	if sv == nil {
		t.Fatal("CollectStats sv = nil, want non-nil envelope even on partial failure")
	}
	if !sv.Enabled {
		t.Error("Enabled = false, want true (backend is non-nil)")
	}
	if sv.BuildingGeneration != nil {
		t.Errorf("BuildingGeneration = %+v, want nil (lookup failed)", sv.BuildingGeneration)
	}
}

func TestCollectStats_BuildingStatsError_Tolerated(t *testing.T) {
	// BuildingGeneration loads fine, but its Stats call fails. The
	// envelope is still returned with BuildingGeneration=nil, and the
	// stats failure is joined into the returned error so callers can log
	// it. The pending total excludes the unmeasured building generation.
	wantErr := errors.New("stats table locked")
	b := &statsFakeBackend{
		building: &Generation{
			ID:        2,
			Model:     "nomic-embed",
			Dimension: 768,
		},
		statsErr: map[GenerationID]error{2: wantErr},
	}

	sv, err := CollectStats(context.Background(), b)
	if err == nil {
		t.Fatalf("CollectStats err = nil, want wrapping %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("CollectStats err = %v, want to wrap %v", err, wantErr)
	}
	if sv == nil {
		t.Fatal("CollectStats sv = nil, want non-nil envelope")
	}
	if sv.BuildingGeneration != nil {
		t.Errorf("BuildingGeneration = %+v, want nil (Stats failed)", sv.BuildingGeneration)
	}
	if sv.PendingEmbeddingsTotal != 0 {
		t.Errorf("PendingEmbeddingsTotal = %d, want 0", sv.PendingEmbeddingsTotal)
	}
}

func TestCollectStats_StatsError_Tolerated(t *testing.T) {
	// Active generation loads fine, but Stats(active.ID) fails. The
	// helper returns a StatsView with ActiveGeneration=nil and a joined
	// error so callers can log the failure without losing the envelope.
	wantErr := errors.New("stats table locked")
	b := &statsFakeBackend{
		active: &Generation{
			ID:          5,
			Model:       "nomic-embed",
			Dimension:   768,
			Fingerprint: "nomic-embed:768",
			State:       GenerationActive,
		},
		statsErr: map[GenerationID]error{5: wantErr},
	}

	sv, err := CollectStats(context.Background(), b)
	if err == nil {
		t.Fatalf("CollectStats err = nil, want wrapping %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("CollectStats err = %v, want to wrap %v", err, wantErr)
	}
	if sv == nil {
		t.Fatal("CollectStats sv = nil, want non-nil envelope")
	}
	if !sv.Enabled {
		t.Error("Enabled = false, want true (backend is non-nil)")
	}
	if sv.ActiveGeneration != nil {
		t.Errorf("ActiveGeneration = %+v, want nil (Stats failed)", sv.ActiveGeneration)
	}
	if sv.PendingEmbeddingsTotal != 0 {
		t.Errorf("PendingEmbeddingsTotal = %d, want 0 (no successful stats)",
			sv.PendingEmbeddingsTotal)
	}
}
