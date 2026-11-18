// Package remotecredential obtains short-lived bearers for a remote msgvault
// server from a trusted local credential helper. Bearers stay in process
// memory. A journal on disk records each request before the helper starts, so
// an exchange whose outcome is unknown is replayed exactly rather than issued
// again under a new identity. The journal never holds a credential.
package remotecredential

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

// renewMargin is how long before expiry a held bearer stops being used, so a
// request does not reach the server just after its bearer expires.
const renewMargin = 30 * time.Second

// lockRetry is how often a command waiting for another command's exchange
// checks the journal lock again.
const lockRetry = 50 * time.Millisecond

// Source supplies bearers for one remote. Token is safe for concurrent use.
type Source struct {
	argv        []string
	scope       credentialprovider.Scope
	journalPath string

	mu   sync.Mutex
	held *held
}

type held struct {
	bearer  string
	lineage lineage
	started time.Time
	usable  time.Duration
}

// lineage identifies the newest credential the helper committed for this
// client, which the next renew or replace names as its predecessor.
type lineage struct {
	RequestID    uuid.UUID `json:"request_id"`
	CredentialID uuid.UUID `json:"credential_id"`
	Generation   int64     `json:"credential_generation"`
}

type journal struct {
	Scope   credentialprovider.Scope    `json:"scope"`
	Pending *credentialprovider.Request `json:"pending,omitzero"`
	Current *lineage                    `json:"current,omitzero"`
}

// New checks argv and scope without starting the helper. The journal for scope
// lives in dir, one file per delegation.
func New(argv []string, scope credentialprovider.Scope, dir string) (*Source, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("credential_provider needs a command")
	}
	probe, err := json.Marshal(credentialprovider.Request{Scope: scope, Version: 1, Operation: "authorize", RequestID: uuid.New()})
	if err != nil {
		return nil, err
	}
	if _, err := credentialprovider.DecodeRequest(bytes.NewReader(probe)); err != nil {
		return nil, fmt.Errorf("credential_provider needs an https url and a delegation with archive_id, client_id, delegation_id, and distinct positive source_ids: %w", err)
	}
	return &Source{argv: argv, scope: scope, journalPath: filepath.Join(dir, scope.DelegationID.String()+".json")}, nil
}

// Token returns the held bearer while it is usable, and otherwise asks the
// helper for a new one.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held.valid(time.Now()) {
		return s.held.bearer, nil
	}
	previous := s.held
	s.held = nil
	return s.exchange(ctx, previous)
}

// exchange serializes with other commands through the journal lock. A replay
// that reports a committed credential without its secret is followed by one
// replace, so a call runs the helper at most twice.
func (s *Source) exchange(ctx context.Context, previous *held) (string, error) {
	if err := fileutil.SecureMkdirAll(filepath.Dir(s.journalPath), 0o700); err != nil {
		return "", fmt.Errorf("create credential journal directory: %w", err)
	}
	lock := flock.New(s.journalPath + ".lock")
	if _, err := lock.TryLockContext(ctx, lockRetry); err != nil {
		return "", fmt.Errorf("lock credential journal: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	j, err := s.load()
	if err != nil {
		return "", err
	}
	for range 2 {
		request := j.Pending
		if request == nil {
			request = s.next(j.Current, previous)
			j.Pending = request
			if err := s.save(j); err != nil {
				return "", err
			}
		}
		started := time.Now()
		response, err := credentialprovider.Exchange(ctx, s.argv, *request)
		if errors.Is(err, credentialprovider.ErrInvalidRequest) {
			// The helper refused the input before any remote operation, so
			// replaying it cannot help.
			j.Pending = nil
			return "", errors.Join(err, s.save(j))
		}
		if err != nil {
			// The outcome is unknown. The saved request is replayed next time.
			return "", fmt.Errorf("run credential helper: %w", err)
		}
		j.Pending = nil
		switch response.Status {
		case credentialprovider.StatusReady, credentialprovider.StatusAlreadyIssued, credentialprovider.StatusReplacementRequired:
			j.Current = &lineage{RequestID: request.RequestID, CredentialID: response.CredentialID, Generation: response.CredentialGeneration}
		case credentialprovider.StatusConflict:
			// The lineage no longer matches the helper's records; the next
			// command starts a new authorization rather than guessing.
			j.Current = nil
		default:
			// Other refusals leave the lineage for the next request.
		}
		if err := s.save(j); err != nil {
			return "", err
		}
		switch response.Status {
		case credentialprovider.StatusReady:
			lifetime := response.ExpiresAt.Sub(response.IssuedAt)
			s.held = &held{
				bearer:  response.Credential.Token,
				lineage: *j.Current,
				started: started,
				usable:  lifetime - min(lifetime/2, renewMargin),
			}
			if !s.held.valid(time.Now()) {
				return "", credentialprovider.ErrCredentialExpired
			}
			return s.held.bearer, nil
		case credentialprovider.StatusAlreadyIssued, credentialprovider.StatusReplacementRequired:
			// An earlier command received this credential and exited; replace it.
			continue
		default:
			return "", &credentialprovider.RefusalError{Status: response.Status, Reason: response.Reason}
		}
	}
	return "", errors.New("credential helper replayed a credential instead of issuing a replacement")
}

// next builds the request that follows current: renew for the bearer this
// process held, replace for one another process held, else authorize.
func (s *Source) next(current *lineage, previous *held) *credentialprovider.Request {
	request := &credentialprovider.Request{Scope: s.scope, Version: 1, Operation: "authorize", RequestID: uuid.New()}
	if current != nil {
		request.Operation = "replace"
		if previous != nil && previous.lineage == *current {
			request.Operation = "renew"
		}
		request.PreviousRequestID = current.RequestID
		request.CredentialID = current.CredentialID
		request.ExpectedGeneration = current.Generation
	}
	return request
}

// load reads the journal. A journal for another scope, such as one replaced
// when the hosting service issued a new delegation, is discarded.
func (s *Source) load() (journal, error) {
	data, err := os.ReadFile(s.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return journal{Scope: s.scope}, nil
	}
	if err != nil {
		return journal{}, fmt.Errorf("read credential journal: %w", err)
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return journal{}, fmt.Errorf("read credential journal %s: %w", s.journalPath, err)
	}
	if !sameScope(j.Scope, s.scope) {
		return journal{Scope: s.scope}, nil
	}
	return j, nil
}

func (s *Source) save(j journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("encode credential journal: %w", err)
	}
	if err := fileutil.SecureReplaceFile(s.journalPath, data, 0o600); err != nil {
		return fmt.Errorf("save credential journal: %w", err)
	}
	return nil
}

func sameScope(a, b credentialprovider.Scope) bool {
	return a.Endpoint == b.Endpoint && a.ArchiveID == b.ArchiveID && a.ClientID == b.ClientID &&
		a.DelegationID == b.DelegationID && slices.Equal(a.SourceIDs, b.SourceIDs) &&
		slices.Equal(a.Tools, b.Tools) && slices.Equal(a.Operations, b.Operations)
}

// valid bounds both monotonic and wall-clock elapsed time. The monotonic clock
// may stop while the host sleeps; the wall clock does not.
func (h *held) valid(now time.Time) bool {
	return h != nil && now.Sub(h.started) < h.usable && now.Round(0).Sub(h.started.Round(0)) < h.usable
}
