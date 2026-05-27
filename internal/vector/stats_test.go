package vector

import (
	"context"
	"errors"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	requirepkg.NoError(t, err, "CollectStats(nil)")
	assertpkg.Nil(t, sv, "CollectStats(nil) should return nil")
}

func TestCollectStats_ActiveOnly(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "CollectStats")
	require.NotNil(sv, "CollectStats returned nil StatsView")
	assert.True(sv.Enabled)
	require.NotNil(sv.ActiveGeneration, "ActiveGeneration is nil")
	ag := sv.ActiveGeneration
	assert.Equal(GenerationID(5), ag.ID)
	assert.Equal("nomic-embed", ag.Model)
	assert.Equal(768, ag.Dimension)
	assert.Equal("nomic-embed:768", ag.Fingerprint)
	assert.Equal("active", ag.State)
	assert.Equal(int64(100), ag.MessageCount)
	assert.Equal(activatedAt.Format(time.RFC3339), ag.ActivatedAt)
	assert.Nil(sv.BuildingGeneration)
	assert.Equal(int64(7), sv.PendingEmbeddingsTotal)
}

func TestCollectStats_BuildingOnly(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "CollectStats")
	require.NotNil(sv, "CollectStats returned nil StatsView")
	assert.True(sv.Enabled)
	assert.Nil(sv.ActiveGeneration)
	require.NotNil(sv.BuildingGeneration, "BuildingGeneration is nil")
	bg := sv.BuildingGeneration
	assert.Equal(GenerationID(9), bg.ID)
	assert.Equal("nomic-embed", bg.Model)
	assert.Equal(768, bg.Dimension)
	assert.Equal(startedAt.Format(time.RFC3339), bg.StartedAt)
	assert.Equal(int64(40), bg.Progress.Done)
	assert.Equal(int64(100), bg.Progress.Total)
	assert.Equal(int64(60), sv.PendingEmbeddingsTotal)
}

func TestCollectStats_BothGenerations(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "CollectStats")
	require.NotNil(sv, "CollectStats returned nil StatsView")
	if assert.NotNil(sv.ActiveGeneration) {
		assert.Equal(GenerationID(1), sv.ActiveGeneration.ID)
	}
	if assert.NotNil(sv.BuildingGeneration) {
		assert.Equal(GenerationID(2), sv.BuildingGeneration.ID)
	}
	// Sum of both pending counts: 3 + 450.
	assert.Equal(int64(453), sv.PendingEmbeddingsTotal)
}

func TestCollectStats_ActiveError(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// A non-sentinel error from ActiveGeneration is joined into the
	// returned error, but the envelope (Enabled=true) is still returned
	// so callers can log the failure and still render whatever partial
	// stats came back (e.g. a building generation).
	wantErr := errors.New("db connection refused")
	b := &statsFakeBackend{activeErr: wantErr}

	sv, err := CollectStats(context.Background(), b)
	require.Error(err, "CollectStats err should wrap want")
	require.ErrorIs(err, wantErr)
	require.NotNil(sv, "CollectStats sv = nil, want non-nil envelope even on partial failure")
	assert.True(sv.Enabled, "backend is non-nil")
	assert.Nil(sv.ActiveGeneration, "lookup failed")
}

func TestCollectStats_BuildingError(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// A non-sentinel error from BuildingGeneration is joined into the
	// returned error, symmetric with the ActiveGeneration error path.
	wantErr := errors.New("db connection refused")
	b := &statsFakeBackend{buildErr: wantErr}

	sv, err := CollectStats(context.Background(), b)
	require.Error(err, "CollectStats err should wrap want")
	require.ErrorIs(err, wantErr)
	require.NotNil(sv, "CollectStats sv = nil, want non-nil envelope even on partial failure")
	assert.True(sv.Enabled, "backend is non-nil")
	assert.Nil(sv.BuildingGeneration, "lookup failed")
}

func TestCollectStats_BuildingStatsError_Tolerated(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.Error(err, "CollectStats err should wrap want")
	require.ErrorIs(err, wantErr)
	require.NotNil(sv, "CollectStats sv = nil, want non-nil envelope")
	assert.Nil(sv.BuildingGeneration, "Stats failed")
	assert.Equal(int64(0), sv.PendingEmbeddingsTotal)
}

func TestCollectStats_StatsError_Tolerated(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.Error(err, "CollectStats err should wrap want")
	require.ErrorIs(err, wantErr)
	require.NotNil(sv, "CollectStats sv = nil, want non-nil envelope")
	assert.True(sv.Enabled, "backend is non-nil")
	assert.Nil(sv.ActiveGeneration, "Stats failed")
	assert.Equal(int64(0), sv.PendingEmbeddingsTotal, "no successful stats")
}
