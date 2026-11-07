package businessreviews

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Targets  func(context.Context) ([]Target, error)
	Provider func(context.Context, Target) (Provider, string, error)
}
type pendingReply struct {
	principal string
	target    Target
	binding   string
	review    Review
	comment   string
	receiptID string
	expires   time.Time
}
type storedReceipt struct {
	principal string
	receipt   Receipt
	expires   time.Time
}

// Service owns only ephemeral state. It has no Store, config or OAuth dependency.
// One service belongs to one server, never a process-wide singleton.
type Service struct {
	options      Options
	mu           sync.Mutex
	pending      map[string]pendingReply
	receipts     map[string]storedReceipt
	writeGate    chan struct{}
	now          func() time.Time
	stop         chan struct{}
	done         chan struct{}
	closed       bool
	generation   uint64
	active       sync.WaitGroup
	activeCancel context.CancelFunc
}

func NewService(options Options) *Service {
	s := &Service{options: options, pending: make(map[string]pendingReply), receipts: make(map[string]storedReceipt), writeGate: make(chan struct{}, 1), now: time.Now, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-timer.C:
				s.expire()
			}
		}
	}()
	return s
}
func (s *Service) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		clear(s.pending)
		close(s.stop)
	}
	cancel := s.activeCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Confirmation admission and Add are serialized with closed under mu.
	// Retain receipts until any already-admitted write has settled.
	s.active.Wait()
	s.mu.Lock()
	clear(s.receipts)
	s.mu.Unlock()
	<-s.done
}
func (s *Service) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
}
func (s *Service) expireLocked() {
	now := s.now()
	for token, p := range s.pending {
		if !now.Before(p.expires) {
			delete(s.pending, token)
			s.setStatusLocked(p.receiptID, "failed")
		}
	}
	for id, r := range s.receipts {
		if !now.Before(r.expires) {
			delete(s.receipts, id)
		}
	}
}
func (s *Service) setStatusLocked(id, status string) *Receipt {
	r, ok := s.receipts[id]
	if !ok {
		return nil
	}
	r.receipt.Status = status
	r.receipt.UpdatedAt = s.now()
	s.receipts[id] = r
	receipt := r.receipt
	return &receipt
}
func (s *Service) target(ctx context.Context, name string) (Target, error) {
	targets, err := s.options.Targets(ctx)
	if err != nil {
		return Target{}, ErrUnavailable
	}
	for _, target := range targets {
		if target.Name == name {
			if target.Validate() != nil || !target.Enabled {
				return Target{}, ErrDenied
			}
			return target, nil
		}
	}
	return Target{}, ErrDenied
}
func (s *Service) provider(ctx context.Context, t Target) (Provider, string, error) {
	if s.options.Provider == nil {
		return nil, "", ErrUnavailable
	}
	p, binding, err := s.options.Provider(ctx, t)
	if err != nil {
		for _, known := range []error{ErrCredentials, ErrPermission, ErrQuota, ErrTemporary, ErrRejected} {
			if errors.Is(err, known) {
				return nil, "", known
			}
		}
		return nil, "", ErrUnavailable
	}
	if p == nil || binding == "" {
		return nil, "", ErrUnavailable
	}
	return p, binding, nil
}
func (s *Service) Execute(ctx context.Context, principal string, request Request) (*Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.expireLocked()
	closed := s.closed
	generation := s.generation
	s.mu.Unlock()
	if closed {
		return nil, ErrUnavailable
	}
	if request.Action == "capabilities" {
		result := &Result{Capabilities: []Capability{{Provider: "google", Available: true, Read: true, Reply: true, Reason: "Requires the owner's approved own-use project and explicit reply consent"}, {Provider: "facebook", Reason: "Current official ratings access requirements and review-specific owner reply contract could not be verified"}}}
		targets, err := s.options.Targets(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		for _, target := range targets {
			result.Targets = append(result.Targets, TargetInfo{Name: target.Name, Provider: target.Provider, Enabled: target.Enabled, RepliesEnabled: target.RepliesEnabled})
		}
		return result, nil
	}
	if request.Action == "invalidate_target" {
		if principal == "" {
			return nil, ErrDenied
		}
		return s.invalidate(ctx, request.Target)
	}
	if request.Action == "receipt" {
		if principal == "" {
			return nil, ErrDenied
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.receipts[request.ReceiptID]
		if !ok {
			return nil, ErrNotFound
		}
		if r.principal != principal {
			return nil, ErrDenied
		}
		receipt := r.receipt
		return &Result{Receipt: &receipt}, nil
	}
	if request.Action == "confirm_reply" {
		if principal == "" {
			return nil, ErrDenied
		}
		return s.confirm(ctx, principal, request.ConfirmationToken)
	}
	target, err := s.target(ctx, request.Target)
	if err != nil {
		return nil, err
	}
	if request.Action == "preview_reply" && (principal == "" || !target.RepliesEnabled) {
		return nil, ErrDenied
	}
	provider, binding, err := s.provider(ctx, target)
	if err != nil {
		return nil, err
	}
	if request.Action == "list" {
		page, err := provider.List(ctx, target.Location, request.PageToken, request.PageSize)
		if err != nil {
			return nil, err
		}
		return &Result{Reviews: page.Reviews, NextPageToken: page.NextPageToken}, nil
	}
	name := target.Location + "/reviews/" + request.ReviewID
	review, err := provider.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if review.Name != name || review.ReviewID != request.ReviewID {
		return nil, ErrUnavailable
	}
	if request.Action == "get" {
		return &Result{Review: &review}, nil
	}
	preview := &Preview{Review: review, Comment: request.Comment}
	if request.Action == "assist_reply" {
		return &Result{Preview: preview}, nil
	}
	token, id := randomID(), randomID()
	if token == "" || id == "" {
		return nil, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.closed {
		return nil, ErrUnavailable
	}
	if generation != s.generation {
		return nil, ErrConflict
	}
	if len(s.pending) >= 128 || len(s.receipts) >= 256 {
		return nil, ErrCapacity
	}
	now := s.now()
	preview.ExpiresAt = now.Add(5 * time.Minute)
	preview.ConfirmationToken = token
	preview.ReceiptID = id
	s.pending[token] = pendingReply{principal: principal, target: target, binding: binding, review: cloneReview(review), comment: request.Comment, receiptID: id, expires: preview.ExpiresAt}
	s.receipts[id] = storedReceipt{principal: principal, receipt: Receipt{ID: id, Status: "pending_confirmation", CreatedAt: now, UpdatedAt: now}, expires: now.Add(10 * time.Minute)}
	return &Result{Preview: preview}, nil
}
func randomID() string {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(data[:])
}
func (s *Service) confirm(ctx context.Context, principal, token string) (*Result, error) {
	// A single write gate also serializes aliases of the same canonical review.
	// Keeping this global is deliberately smaller than an unbounded lock registry.
	select {
	case s.writeGate <- struct{}{}:
		defer func() { <-s.writeGate }()
	case <-ctx.Done():
		s.cancelPending(principal, token)
		return nil, ErrConflict
	}
	s.mu.Lock()
	s.expireLocked()
	p, ok := s.pending[token]
	if !ok || s.closed {
		s.mu.Unlock()
		return nil, ErrConflict
	}
	if p.principal != principal {
		s.mu.Unlock()
		return nil, ErrDenied
	}
	delete(s.pending, token)
	if ctx.Err() != nil {
		s.setStatusLocked(p.receiptID, "failed")
		s.mu.Unlock()
		return nil, ErrConflict
	}
	ctx, cancel := context.WithCancel(ctx)
	s.active.Add(1)
	s.activeCancel = cancel
	defer func() {
		cancel()
		s.mu.Lock()
		s.activeCancel = nil
		s.mu.Unlock()
		s.active.Done()
	}()
	s.setStatusLocked(p.receiptID, "submitting")
	s.mu.Unlock()
	fail := func(err error) (*Result, error) {
		s.mu.Lock()
		s.setStatusLocked(p.receiptID, "failed")
		s.mu.Unlock()
		return nil, err
	}
	target, err := s.target(ctx, p.target.Name)
	if err != nil || target != p.target || !target.RepliesEnabled {
		return fail(ErrConflict)
	}
	provider, binding, err := s.provider(ctx, target)
	if err != nil || binding != p.binding {
		return fail(ErrConflict)
	}
	review, err := provider.Get(ctx, p.review.Name)
	if err != nil || !reflect.DeepEqual(review, p.review) {
		return fail(ErrConflict)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || ctx.Err() != nil {
		return fail(ErrConflict)
	}
	// Provider APIs have no conditional reply write: an external writer can still
	// change the review between this fresh GET and PUT.
	_, err = provider.UpdateReply(ctx, p.review.Name, p.comment)
	status := "submitted"
	if errors.Is(err, ErrUnknown) || ctx.Err() != nil {
		status = "unknown"
	} else if err != nil {
		status = "failed"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt := s.setStatusLocked(p.receiptID, status)
	if status == "unknown" || status == "submitted" {
		s.generation++
		for otherToken, other := range s.pending {
			if other.review.Name == p.review.Name {
				delete(s.pending, otherToken)
				s.setStatusLocked(other.receiptID, "failed")
			}
		}
	}
	if receipt == nil {
		return nil, ErrUnavailable
	}
	return &Result{Receipt: receipt}, nil
}

func cloneReview(review Review) Review {
	if review.ReviewReply != nil {
		reply := *review.ReviewReply
		review.ReviewReply = &reply
	}
	return review
}
func (s *Service) cancelPending(principal, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending, ok := s.pending[token]; ok && pending.principal == principal {
		delete(s.pending, token)
		s.setStatusLocked(pending.receiptID, "failed")
	}
}

func (s *Service) invalidate(ctx context.Context, name string) (*Result, error) {
	select {
	case s.writeGate <- struct{}{}:
		defer func() { <-s.writeGate }()
	case <-ctx.Done():
		return nil, ErrConflict
	}
	// Resolve configuration even when only an alias has issued consent. Disabled
	// setup entries still identify the credential namespace being disconnected.
	if s.options.Targets == nil {
		return nil, ErrUnavailable
	}
	targets, err := s.options.Targets(ctx)
	if err != nil {
		return nil, ErrUnavailable
	}
	var selected Target
	for _, target := range targets {
		if target.Name == name {
			selected = target
			break
		}
	}
	if selected.Name == "" || selected.Validate() != nil {
		return nil, ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrUnavailable
	}
	s.generation++
	for token, pending := range s.pending {
		sameLocation := selected.Location != "" && pending.target.Provider == selected.Provider && pending.target.Location == selected.Location
		sameCredential := pending.target.Provider == selected.Provider && pending.target.OAuthApp == selected.OAuthApp && strings.EqualFold(pending.target.Email, selected.Email)
		if pending.target.Name == name || sameLocation || sameCredential {
			delete(s.pending, token)
			s.setStatusLocked(pending.receiptID, "failed")
		}
	}
	return &Result{}, nil
}
