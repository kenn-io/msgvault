package providercredentials

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreUsesOwnerOnlyAtomicPublicationAndSeparateETag(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")

	empty, err := Read(dir)
	requirements.NoError(err)
	requirements.NotEmpty(empty.ETag)

	written, err := Put(dir, empty.ETag, VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "stored-secret")
	requirements.NoError(err)
	assertions.NotEqual(empty.ETag, written.ETag)
	if runtime.GOOS != "windows" {
		dirInfo, statErr := os.Stat(dir)
		requirements.NoError(statErr)
		assertions.Equal(os.FileMode(0o700), dirInfo.Mode().Perm())
		fileInfo, statErr := os.Stat(filepath.Join(dir, Filename))
		requirements.NoError(statErr)
		assertions.Equal(os.FileMode(0o600), fileInfo.Mode().Perm())
	}

	loaded, err := Read(dir)
	requirements.NoError(err)
	assertions.Equal(written.ETag, loaded.ETag)
	value, state, err := loaded.Resolve(VectorEmbeddingsID,
		"https://embeddings.example.test/v2", "TEXT_KEY", func(string) (string, bool) {
			return "environment-secret", true
		})
	requirements.NoError(err)
	assertions.Equal("stored-secret", value)
	assertions.Equal(State{Configured: true, Source: SourceStored}, state)

	_, err = Delete(dir, empty.ETag, VectorEmbeddingsID)
	requirements.ErrorIs(err, ErrConflict)
	cleared, err := Delete(dir, loaded.ETag, VectorEmbeddingsID)
	requirements.NoError(err)
	value, state, err = cleared.Resolve(VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "TEXT_KEY", func(string) (string, bool) {
			return "environment-secret", true
		})
	requirements.NoError(err)
	assertions.Equal("environment-secret", value)
	assertions.Equal(State{Configured: true, Source: SourceEnvironment}, state)
}

func TestDeleteSuppressionIfValuePreservesConcurrentCredentialsAndReplacementKeys(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	empty, err := Read(dir)
	requirements.NoError(err)
	withSuppression, err := PutSuppression(dir, empty.ETag, "generated-suppression-key")
	requirements.NoError(err)
	_, err = Put(dir, withSuppression.ETag, VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "stored-provider-key")
	requirements.NoError(err)

	rolledBack, err := DeleteSuppressionIfValue(dir, "generated-suppression-key")
	requirements.NoError(err)
	_, configured, err := rolledBack.ResolveSuppression()
	requirements.NoError(err)
	assertions.False(configured)
	value, state, err := rolledBack.Resolve(VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "", nil)
	requirements.NoError(err)
	assertions.Equal("stored-provider-key", value)
	assertions.Equal(SourceStored, state.Source)

	replacement, err := PutSuppression(dir, rolledBack.ETag, "replacement-suppression-key")
	requirements.NoError(err)
	_, err = DeleteSuppressionIfValue(dir, "generated-suppression-key")
	requirements.ErrorIs(err, ErrConflict)
	stored, configured, err := replacement.ResolveSuppression()
	requirements.NoError(err)
	assertions.True(configured)
	assertions.Equal("replacement-suppression-key", stored)
}

func TestStoreBindsCredentialsToEndpointOrigin(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	empty, err := Read(dir)
	requirements.NoError(err)
	written, err := Put(dir, empty.ETag, VectorMultimodalID,
		"HTTPS://API.VOYAGEAI.COM:443/v1", "voyage-secret")
	requirements.NoError(err)

	value, state, err := written.Resolve(VectorMultimodalID,
		"https://api.voyageai.com/v2", "VOYAGE_API_KEY", nil)
	requirements.NoError(err)
	assertions.Equal("voyage-secret", value, "paths on the same origin may share the credential")
	assertions.Equal(SourceStored, state.Source)
	value, state, err = written.Resolve(VectorMultimodalID,
		"https://api.voyageai.com/v3", "VOYAGE_API_KEY", nil)
	requirements.NoError(err)
	assertions.Equal("voyage-secret", value, "case, default ports, and paths must normalize to one origin")
	assertions.Equal(SourceStored, state.Source)

	value, state, err = written.Resolve(VectorMultimodalID,
		"https://other.example.test/v1", "VOYAGE_API_KEY", func(string) (string, bool) {
			return "environment-must-not-be-used", true
		})
	requirements.ErrorIs(err, ErrOriginMismatch)
	assertions.Empty(value)
	assertions.Equal(State{Configured: false, Source: SourceNone}, state)
}

func TestEndpointOriginRejectsMalformedURLWithoutPanicking(t *testing.T) {
	assertions := assert.New(t)

	origin, err := EndpointOrigin("%")

	assertions.Empty(origin)
	assertions.EqualError(err, "provider endpoint must be an http or https URL with a host")
}

func TestStoreRejectsCorruptOrUnsafePublicationWithoutEnvironmentFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL rejection is covered by the native permission backend tests")
	}
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{name: "corrupt", content: `{"version":1,"credentials":`, mode: 0o600},
		{name: "unsafe mode", content: `{"version":1,"credentials":{}}`, mode: 0o644},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			dir := filepath.Join(t.TempDir(), "tokens")
			requirements.NoError(os.MkdirAll(dir, 0o700))
			requirements.NoError(os.WriteFile(filepath.Join(dir, Filename), []byte(tt.content), tt.mode))
			snapshot, err := Read(dir)
			requirements.Error(err)
			_, _, resolveErr := snapshot.Resolve(VectorEmbeddingsID,
				"https://embeddings.example.test/v1", "TEXT_KEY", func(string) (string, bool) {
					return "must-not-fallback", true
				})
			assert.ErrorIs(t, resolveErr, ErrUnavailable)
		})
	}
}

func TestStoreRejectsSymlinkAndWrongOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reparse-point and owner checks use the native DACL backend")
	}
	t.Run("symlink", func(t *testing.T) {
		requirements := require.New(t)
		dir := filepath.Join(t.TempDir(), "tokens")
		requirements.NoError(os.MkdirAll(dir, 0o700))
		target := filepath.Join(t.TempDir(), "private.json")
		requirements.NoError(os.WriteFile(target, []byte(`{"version":1,"credentials":{}}`), 0o600))
		requirements.NoError(os.Symlink(target, filepath.Join(dir, Filename)))
		_, err := Read(dir)
		assert.ErrorIs(t, err, ErrUnavailable)
	})

	if os.Geteuid() != 0 {
		return
	}
	t.Run("wrong owner", func(t *testing.T) {
		requirements := require.New(t)
		dir := filepath.Join(t.TempDir(), "tokens")
		requirements.NoError(os.MkdirAll(dir, 0o700))
		path := filepath.Join(dir, Filename)
		requirements.NoError(os.WriteFile(path, []byte(`{"version":1,"credentials":{}}`), 0o600))
		requirements.NoError(os.Chown(path, 65534, 65534))
		_, err := Read(dir)
		assert.ErrorIs(t, err, ErrUnavailable)
	})
}

func TestStoreSerializesSameETagWriters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	empty, err := Read(dir)
	requirements.NoError(err)
	type outcome struct{ err error }
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, value := range []string{"first", "second"} {
		go func(value string) {
			<-start
			_, writeErr := Put(dir, empty.ETag, VectorEmbeddingsID,
				"https://embeddings.example.test/v1", value)
			results <- outcome{err: writeErr}
		}(value)
	}
	close(start)
	first, second := <-results, <-results
	assertions.NotEqual(first.err == nil, second.err == nil, "exactly one same-ETag writer must publish")
	if first.err != nil {
		requirements.ErrorIs(first.err, ErrConflict)
	}
	if second.err != nil {
		requirements.ErrorIs(second.err, ErrConflict)
	}

	raw, err := os.ReadFile(filepath.Join(dir, Filename))
	requirements.NoError(err)
	assertions.Contains(string(raw), `"id":"vector.embeddings"`)
	assertions.Contains(string(raw), `"kind":"vector_embeddings"`)
	assertions.Contains(string(raw), `"revision":1`)
}

func TestCredentialIDsAreStableAndValidated(t *testing.T) {
	for _, id := range []string{
		VectorEmbeddingsID,
		VectorMultimodalID,
		PeopleSweepID,
		PersonEnrichmentID("exa-primary"),
	} {
		require.NoError(t, ValidateID(id), id)
	}
	for _, id := range []string{"vector.text", "people.enrichment/", "people.enrichment/../secret", "unknown"} {
		assert.Error(t, ValidateID(id), id)
	}
}
