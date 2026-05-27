package vector

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackend implements Backend for ResolveActiveForFingerprint tests.
// Only ActiveGeneration and BuildingGeneration are exercised.
type fakeBackend struct {
	active    *Generation
	building  *Generation
	activeErr error
	buildErr  error
}

func (f *fakeBackend) CreateGeneration(context.Context, string, int, string) (GenerationID, error) {
	return 0, errors.New("not implemented")
}
func (f *fakeBackend) ActivateGeneration(context.Context, GenerationID) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) RetireGeneration(context.Context, GenerationID) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) ActiveGeneration(context.Context) (Generation, error) {
	if f.activeErr != nil {
		return Generation{}, f.activeErr
	}
	if f.active == nil {
		return Generation{}, ErrNoActiveGeneration
	}
	return *f.active, nil
}
func (f *fakeBackend) BuildingGeneration(context.Context) (*Generation, error) {
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return f.building, nil
}
func (f *fakeBackend) Upsert(context.Context, GenerationID, []Chunk) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) Search(context.Context, GenerationID, []float32, int, Filter) ([]Hit, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeBackend) Delete(context.Context, GenerationID, []int64) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) Stats(context.Context, GenerationID) (Stats, error) {
	return Stats{}, errors.New("not implemented")
}
func (f *fakeBackend) LoadVector(context.Context, int64) ([]float32, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeBackend) Close() error { return nil }
func (f *fakeBackend) EnsureSeeded(context.Context, GenerationID) error {
	return errors.New("not implemented")
}

func TestResolveActiveForFingerprint_Matches(t *testing.T) {
	b := &fakeBackend{active: &Generation{ID: 1, Fingerprint: "m:768:p1-111111"}}
	g, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	require.NoError(t, err, "ResolveActiveForFingerprint")
	assert.Equal(t, "m:768:p1-111111", g.Fingerprint)
	assert.Equal(t, GenerationID(1), g.ID)
}

func TestResolveActiveForFingerprint_Stale(t *testing.T) {
	b := &fakeBackend{active: &Generation{Fingerprint: "m:768:p1-111111"}}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:1024:p1-111111")
	assert.ErrorIs(t, err, ErrIndexStale)
}

// TestResolveActiveForFingerprint_StaleOnPreprocessFlip pins the
// upgrade path that motivated folding preprocess into the fingerprint:
// flipping a strip_* toggle (here strip_html → false) must surface as
// ErrIndexStale, not silently top up the existing index with vectors
// built under the new sanitization policy.
func TestResolveActiveForFingerprint_StaleOnPreprocessFlip(t *testing.T) {
	b := &fakeBackend{active: &Generation{Fingerprint: "m:768:p1-111111"}}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-101111")
	assert.ErrorIs(t, err, ErrIndexStale)
}

func TestResolveActiveForFingerprint_NoneAndBuildingReturnsBuildingError(t *testing.T) {
	b := &fakeBackend{building: &Generation{ID: 42, Fingerprint: "m:768:p1-111111"}}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	assert.ErrorIs(t, err, ErrIndexBuilding)
}

func TestResolveActiveForFingerprint_NothingReturnsNotEnabled(t *testing.T) {
	b := &fakeBackend{}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	assert.ErrorIs(t, err, ErrNotEnabled)
}

func TestResolveActiveForFingerprint_BackendError(t *testing.T) {
	wantErr := errors.New("db down")
	b := &fakeBackend{activeErr: wantErr}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	require.Error(t, err, "expected error wrapping db down")
	assert.ErrorIs(t, err, wantErr)
}

func TestResolveActiveForFingerprint_BuildingBackendError(t *testing.T) {
	wantErr := errors.New("building failed")
	b := &fakeBackend{buildErr: wantErr}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	require.Error(t, err, "expected error wrapping building failed")
	assert.ErrorIs(t, err, wantErr)
}
