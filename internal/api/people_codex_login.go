package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

var errPeopleCodexLoginBusy = errors.New("a Codex device login is already active")
var errPeopleCodexLoginPreparation = errors.New("codex device login preparation failed")
var errPeopleCodexLoginCompleted = errors.New("codex device login completed before cancellation")

type peopleCodexLoginClient interface {
	StartDeviceLogin(ctx context.Context, present func(peoplesweep.DeviceLogin) error) error
	ListModels(ctx context.Context) ([]peoplesweep.CodexModel, error)
}

type peopleCodexLoginSession struct {
	draft  peoplesweep.EnrollmentDraft
	owner  string
	name   string
	state  string
	login  peoplesweep.DeviceLogin
	cancel context.CancelFunc
	done   chan struct{}
	result error
}

// peopleCodexLogins owns one active device ceremony for the daemon's single
// private Codex auth home. A completed login keeps blocking new ceremonies
// until its draft is consumed, cancelled, or expired, because its commit
// replaced the shared credential and the owner may still be listing models
// or creating the profile under that account. The draft ID is never a bearer
// credential: every lookup also checks the authenticated caller's session
// identity.
type peopleCodexLogins struct {
	mu       sync.Mutex
	drafts   *peoplesweep.EnrollmentDrafts
	sessions map[string]*peopleCodexLoginSession
	client   peopleCodexLoginClient
}

func newPeopleCodexLogins(client peopleCodexLoginClient, now func() time.Time) *peopleCodexLogins {
	return &peopleCodexLogins{
		drafts: peoplesweep.NewEnrollmentDrafts(now), sessions: make(map[string]*peopleCodexLoginSession),
		client: client,
	}
}

func (m *peopleCodexLogins) Start(owner, name string, beforeStart func() error) (peoplesweep.EnrollmentDraft, error) {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return peoplesweep.EnrollmentDraft{}, err
	}
	if m.client == nil {
		return peoplesweep.EnrollmentDraft{}, errors.New("codex enrollment is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, existing := range m.sessions {
		if _, err := m.drafts.Get(existing.owner, id); err != nil {
			existing.cancel()
			delete(m.sessions, id)
			continue
		}
		if existing.state == "pending" || existing.state == "complete" {
			// A completed but unconsumed login still owns the shared auth
			// home; a second device login would silently replace its account
			// before the profile is created.
			return peoplesweep.EnrollmentDraft{}, errPeopleCodexLoginBusy
		}
		// A failed ceremony never committed a credential, so it cannot
		// conflict with a fresh login.
		existing.cancel()
		delete(m.sessions, id)
	}
	draft, err := m.drafts.Create(owner, "codex")
	if err != nil {
		return peoplesweep.EnrollmentDraft{}, err
	}
	if beforeStart != nil {
		if err := beforeStart(); err != nil {
			_ = m.drafts.Cancel(owner, draft.ID)
			return peoplesweep.EnrollmentDraft{}, errors.Join(errPeopleCodexLoginPreparation, err)
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), draft.ExpiresAt)
	session := &peopleCodexLoginSession{draft: draft, owner: owner, name: name, state: "pending", cancel: cancel, done: make(chan struct{})}
	m.sessions[draft.ID] = session
	go m.run(ctx, session)
	return draft, nil
}

func (m *peopleCodexLogins) run(ctx context.Context, session *peopleCodexLoginSession) {
	defer session.cancel()
	err := m.client.StartDeviceLogin(ctx, func(login peoplesweep.DeviceLogin) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.sessions[session.draft.ID] != session || ctx.Err() != nil {
			return context.Canceled
		}
		session.login = login
		return nil
	})
	m.mu.Lock()
	defer close(session.done)
	defer m.mu.Unlock()
	session.result = err
	if m.sessions[session.draft.ID] != session {
		return
	}
	if err != nil {
		session.state = "failed"
	} else {
		session.state = "complete"
	}
}

func (m *peopleCodexLogins) Get(owner, id string) (peopleCodexLoginSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.drafts.Get(owner, id); err != nil {
		return peopleCodexLoginSession{}, err
	}
	session := m.sessions[id]
	if session == nil || session.owner != owner {
		return peopleCodexLoginSession{}, peoplesweep.ErrEnrollmentDraftNotFound
	}
	snapshot := *session
	snapshot.cancel = nil
	return snapshot, nil
}

func (m *peopleCodexLogins) Cancel(owner, id string) error {
	m.mu.Lock()
	if _, err := m.drafts.Get(owner, id); err != nil {
		m.mu.Unlock()
		return err
	}
	session := m.sessions[id]
	if session == nil || session.owner != owner {
		m.mu.Unlock()
		return peoplesweep.ErrEnrollmentDraftNotFound
	}
	wasPending := session.state == "pending"
	session.cancel()
	m.mu.Unlock()
	<-session.done
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.drafts.Cancel(owner, id); err != nil {
		return err
	}
	delete(m.sessions, id)
	if wasPending && session.result == nil {
		return errPeopleCodexLoginCompleted
	}
	return nil
}
