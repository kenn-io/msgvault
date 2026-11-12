package api

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
)

type roleLockProbeCandidate struct {
	*iCloudCandidateFixture

	mutex *sync.Mutex
}

func (p *roleLockProbeCandidate) SetBookRoles(context.Context, int64, carddav.BookRoles) error {
	if p.mutex.TryLock() {
		p.mutex.Unlock()
		return errors.New("role write reached its delegate without the shared save lock")
	}
	return nil
}

func TestPublishedNonICloudRoleWritesUseSharedSaveLock(t *testing.T) {
	f := newICloudAccountFixture(t, "work")
	candidate := &roleLockProbeCandidate{iCloudCandidateFixture: &iCloudCandidateFixture{fixture: f}, mutex: f.controller.root().saveLock()}
	runtime := f.controller.runtimeForCredential(candidate, carddav.Credential{}, config.CardDAVConfig{})
	require.NoError(t, runtime.SetBookRoles(t.Context(), 1, carddav.BookRoles{}))
}

func cardDAVRuntimeDelegate(operations CardDAVOperations) CardDAVOperations {
	switch runtime := operations.(type) {
	case *cardDAVRoleOperations:
		return runtime.CardDAVOperations
	case *iCloudOperations:
		return runtime.CardDAVOperations
	default:
		return operations
	}
}
