package vector

import (
	"context"
	"errors"
	"fmt"
	"testing"
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
	if err != nil {
		t.Fatalf("ResolveActiveForFingerprint: %v", err)
	}
	if g.Fingerprint != "m:768:p1-111111" {
		t.Errorf("fingerprint = %q, want m:768:p1-111111", g.Fingerprint)
	}
	if g.ID != 1 {
		t.Errorf("ID = %d, want 1", g.ID)
	}
}

func TestResolveActiveForFingerprint_Stale(t *testing.T) {
	b := &fakeBackend{active: &Generation{Fingerprint: "m:768:p1-111111"}}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:1024:p1-111111")
	if !errors.Is(err, ErrIndexStale) {
		t.Errorf("err = %v, want ErrIndexStale", err)
	}
}

// TestResolveActiveForFingerprint_StaleOnPreprocessFlip pins the
// upgrade path that motivated folding preprocess into the fingerprint:
// flipping a strip_* toggle (here strip_html → false) must surface as
// ErrIndexStale, not silently top up the existing index with vectors
// built under the new sanitization policy.
func TestResolveActiveForFingerprint_StaleOnPreprocessFlip(t *testing.T) {
	b := &fakeBackend{active: &Generation{Fingerprint: "m:768:p1-111111"}}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-101111")
	if !errors.Is(err, ErrIndexStale) {
		t.Errorf("err = %v, want ErrIndexStale", err)
	}
}

func TestResolveActiveForFingerprint_NoneAndBuildingReturnsBuildingError(t *testing.T) {
	b := &fakeBackend{building: &Generation{ID: 42, Fingerprint: "m:768:p1-111111"}}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	if !errors.Is(err, ErrIndexBuilding) {
		t.Errorf("err = %v, want ErrIndexBuilding", err)
	}
}

func TestResolveActiveForFingerprint_NothingReturnsNotEnabled(t *testing.T) {
	b := &fakeBackend{}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	if !errors.Is(err, ErrNotEnabled) {
		t.Errorf("err = %v, want ErrNotEnabled", err)
	}
}

func TestResolveActiveForFingerprint_BackendError(t *testing.T) {
	wantErr := fmt.Errorf("db down")
	b := &fakeBackend{activeErr: wantErr}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wraps db down", err)
	}
}

func TestResolveActiveForFingerprint_BuildingBackendError(t *testing.T) {
	wantErr := fmt.Errorf("building failed")
	b := &fakeBackend{buildErr: wantErr}
	_, err := ResolveActiveForFingerprint(context.Background(), b, "m:768:p1-111111")
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wraps building failed", err)
	}
}
