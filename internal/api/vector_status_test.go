package api

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
)

// minimalVectorBackend is a minimal vector.Backend for status tests. Embed the
// interface so only the methods a test touches need implementations; the
// status tests never call any of them.
type minimalVectorBackend struct {
	vector.Backend
}

func testServerOptions(t *testing.T, backend vector.Backend) ServerOptions {
	t.Helper()
	return ServerOptions{
		Config:  &config.Config{},
		Logger:  slog.New(slog.DiscardHandler),
		Backend: backend,
	}
}

func TestVectorStatusDerivedFromOptions(t *testing.T) {
	tests := []struct {
		name string
		opts ServerOptions
		want VectorStatus
	}{
		{"no backend defaults to disabled", testServerOptions(t, nil), VectorStatusDisabled},
		{"backend defaults to ready", testServerOptions(t, &minimalVectorBackend{}), VectorStatusReady},
		{
			"explicit initializing wins",
			func() ServerOptions {
				o := testServerOptions(t, nil)
				o.VectorStatus = VectorStatusInitializing
				return o
			}(),
			VectorStatusInitializing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServerWithOptions(tt.opts)
			status, errMsg := srv.VectorStatus()
			assert.Equal(t, tt.want, status)
			assert.Empty(t, errMsg)
		})
	}
}

func TestSetVectorFeaturesTransitionsToReady(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	backend := &minimalVectorBackend{}
	srv.SetVectorFeatures(nil, backend, vector.Config{})

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
	assert.Empty(t, errMsg)
	_, gotBackend, _ := srv.vectorComponents()
	require.NotNil(t, gotBackend)
}

func TestSetVectorInitErrorTransitionsToError(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	srv.SetVectorInitError(errors.New("migration exploded"))

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusError, status)
	assert.Contains(t, errMsg, "migration exploded")
}

func TestSetVectorFeaturesConcurrentReads(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_, _, _ = srv.vectorComponents()
			_, _ = srv.VectorStatus()
		}
	}()
	srv.SetVectorFeatures(nil, &fakeVectorBackend{}, vector.Config{})
	<-done

	status, _ := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
}
