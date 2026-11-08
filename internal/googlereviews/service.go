package googlereviews

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Options struct {
	Targets  func(context.Context) ([]Target, error)
	Provider func(context.Context, Target) (Provider, string, error)
	Archive  Archive
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
type backfillRun struct {
	target Target
	cancel context.CancelFunc
	done   chan struct{}
}

func credentialKey(target Target) string {
	return target.Provider + "\x00" + target.OAuthApp + "\x00" + strings.ToLower(target.Email)
}

type storedReceipt struct {
	principal string
	receipt   Receipt
	expires   time.Time
}

// Service owns ephemeral reply state and coordinates owner-triggered archive
// backfill through its narrow Archive interface. One service belongs to one
// server, never a process-wide singleton.
type Service struct {
	options            Options
	mu                 sync.Mutex
	pending            map[string]pendingReply
	receipts           map[string]storedReceipt
	writeGate          chan struct{}
	now                func() time.Time
	stop               chan struct{}
	done               chan struct{}
	closed             bool
	generation         uint64
	active             sync.WaitGroup
	activeCancel       context.CancelFunc
	backfillCtx        context.Context
	backfillCancel     context.CancelFunc
	backfillActive     map[string]*backfillRun
	blockedCredentials map[string]string
	backfills          sync.WaitGroup
}

func NewService(options Options) *Service {
	// Backfills outlive their initiating HTTP request and use this service-lifetime
	// context until Close cancels the worker during daemon shutdown.
	backfillCtx, backfillCancel := context.WithCancel(context.Background())
	s := &Service{options: options, pending: make(map[string]pendingReply), receipts: make(map[string]storedReceipt), writeGate: make(chan struct{}, 1), now: time.Now, stop: make(chan struct{}), done: make(chan struct{}), backfillCtx: backfillCtx, backfillCancel: backfillCancel, backfillActive: make(map[string]*backfillRun), blockedCredentials: make(map[string]string)}
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
	backfillCancel := s.backfillCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	backfillCancel()
	// Confirmation admission and Add are serialized with closed under mu.
	// Retain receipts until any already-admitted write has settled.
	s.active.Wait()
	s.backfills.Wait()
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
	target, err := s.configuredTarget(ctx, name)
	if err != nil {
		return Target{}, err
	}
	if !target.Enabled {
		return Target{}, ErrDenied
	}
	return target, nil
}

func (s *Service) configuredTarget(ctx context.Context, name string) (Target, error) {
	if s.options.Targets == nil {
		return Target{}, ErrUnavailable
	}
	targets, err := s.options.Targets(ctx)
	if err != nil {
		return Target{}, ErrUnavailable
	}
	for _, target := range targets {
		if target.Name == name {
			if target.Validate() != nil {
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
	if request.Action == "invalidate_target" || request.Action == "resume_target" {
		if principal == "" {
			return nil, ErrDenied
		}
		return s.invalidate(ctx, request.Target, request.Action == "resume_target", request.ConfirmationToken)
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
	if request.Action == "archive_status" {
		return s.archiveStatus(ctx, request.Target)
	}
	if request.Action == "backfill" {
		if principal == "" {
			return nil, ErrDenied
		}
		return s.startBackfill(ctx, request.Target)
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

const backfillCoverageNote = "Google can omit reviews from later API pages. API pagination exhaustion does not guarantee lifetime coverage."

func (s *Service) archiveStatus(ctx context.Context, targetName string) (*Result, error) {
	if s.options.Archive == nil {
		return nil, ErrArchive
	}
	if _, err := s.configuredTarget(ctx, targetName); err != nil {
		return nil, err
	}
	state, err := s.options.Archive.GoogleReviewsBackfillState(ctx, targetName)
	if err != nil {
		return nil, ErrArchive
	}
	state = withCoverageNote(state)
	return &Result{ArchiveSync: &state}, nil
}

func (s *Service) startBackfill(ctx context.Context, targetName string) (*Result, error) {
	if s.options.Archive == nil {
		return nil, ErrArchive
	}
	target, err := s.target(ctx, targetName)
	if err != nil {
		return nil, err
	}
	key := target.Name + "\x00" + target.Location
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	if s.blockedCredentials[credentialKey(target)] != "" {
		s.mu.Unlock()
		return nil, ErrCredentials
	}
	if s.backfillActive[key] != nil {
		s.mu.Unlock()
		state, err := s.options.Archive.GoogleReviewsBackfillState(ctx, target.Name)
		if err != nil {
			return nil, ErrArchive
		}
		state = withCoverageNote(state)
		return &Result{ArchiveSync: &state}, nil
	}
	workerCtx, cancel := context.WithCancel(s.backfillCtx)
	run := &backfillRun{target: target, cancel: cancel, done: make(chan struct{})}
	s.backfillActive[key] = run
	// Add while holding mu so Close cannot begin Wait before this worker is
	// accounted for.
	s.backfills.Add(1)
	s.mu.Unlock()

	state, err := s.options.Archive.BeginGoogleReviewsBackfill(workerCtx, target.Name, target.Location)
	if err != nil {
		s.finishBackfill(key)
		if errors.Is(err, ErrArchiveBusy) {
			return nil, err
		}
		return nil, ErrArchive
	}
	state = withCoverageNote(state)
	go s.runBackfill(workerCtx, key, target, state)
	return &Result{ArchiveSync: &state}, nil
}

func withCoverageNote(state BackfillState) BackfillState {
	state.CoverageNote = backfillCoverageNote
	return state
}

func (s *Service) runBackfill(ctx context.Context, key string, target Target, state BackfillState) {
	defer s.finishBackfill(key)
	for {
		if state.PagesFetched == math.MaxInt64 {
			s.failBackfill(ctx, target, "page_count_limit")
			return
		}
		if err := ctx.Err(); err != nil {
			s.failBackfill(ctx, target, archiveErrorCode(err))
			return
		}
		current, err := s.target(ctx, target.Name)
		if err != nil || !reflect.DeepEqual(current, target) {
			s.failBackfill(ctx, target, "configuration_changed")
			return
		}
		// Each page gets a freshly checked token so long imports can refresh
		// without reusing a snapshot that has expired or changed accounts.
		provider, _, err := s.provider(ctx, target)
		if err != nil {
			s.failBackfill(ctx, target, archiveErrorCode(err))
			return
		}
		page, err := provider.List(ctx, target.Location, state.NextPageToken, 50)
		if err != nil {
			s.failBackfill(ctx, target, archiveErrorCode(err))
			return
		}
		if err := ctx.Err(); err != nil {
			s.failBackfill(ctx, target, archiveErrorCode(err))
			return
		}
		if err := page.ValidateFor(target.Name, target.Location); err != nil {
			s.failBackfill(ctx, target, "invalid_page")
			return
		}
		state, err = s.options.Archive.ApplyGoogleReviewsPage(ctx, target.Name, target.Location, state.NextPageToken, page)
		if err != nil {
			s.failBackfill(ctx, target, archiveErrorCode(err))
			return
		}
		if state.Status == "api_exhausted" {
			return
		}
	}
}

func (s *Service) finishBackfill(key string) {
	s.mu.Lock()
	run := s.backfillActive[key]
	delete(s.backfillActive, key)
	if run != nil {
		run.cancel()
		close(run.done)
	}
	s.mu.Unlock()
	s.backfills.Done()
}

func archiveErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, ErrQuota):
		return "quota"
	case errors.Is(err, ErrPermission):
		return "permission"
	case errors.Is(err, ErrCredentials):
		return "credentials"
	case errors.Is(err, ErrTemporary):
		return "temporary"
	case errors.Is(err, ErrRejected):
		return "rejected"
	case errors.Is(err, ErrArchiveCycle):
		return "cursor_cycle"
	case errors.Is(err, ErrInvalid):
		return "invalid_page"
	default:
		return "provider_error"
	}
}

func (s *Service) failBackfill(ctx context.Context, target Target, errorCode string) {
	// Persist the resume checkpoint after daemon shutdown cancels the worker;
	// this bounded cleanup must finish after service-context cancellation.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = s.options.Archive.FailGoogleReviewsBackfill(persistCtx, target.Name, target.Location, errorCode)
}

func (s *Service) SearchArchive(ctx context.Context, request ArchiveSearchRequest) (ArchiveSearchResponse, error) {
	if s.options.Archive == nil {
		return ArchiveSearchResponse{}, ErrArchive
	}
	if !utf8.ValidString(request.Query) || strings.TrimSpace(request.Query) == "" || len(request.Query) > 256 ||
		request.Target != "" && !validID(request.Target) {
		return ArchiveSearchResponse{}, ErrSearch
	}
	if request.Page == 0 {
		request.Page = 1
	}
	if request.PageSize == 0 {
		request.PageSize = 20
	}
	if request.Page < 1 || request.Page > 100000 || request.PageSize < 1 || request.PageSize > 100 ||
		request.Page-1 > math.MaxInt/request.PageSize {
		return ArchiveSearchResponse{}, ErrSearch
	}
	response, err := s.options.Archive.SearchGoogleReviews(ctx, request.Query, request.Target, (request.Page-1)*request.PageSize, request.PageSize)
	if err != nil {
		if errors.Is(err, ErrSearch) {
			return ArchiveSearchResponse{}, ErrSearch
		}
		return ArchiveSearchResponse{}, ErrArchive
	}
	response.Query = request.Query
	response.Target = request.Target
	response.Page = request.Page
	response.PageSize = request.PageSize
	return response, nil
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

func (s *Service) invalidate(ctx context.Context, name string, resume bool, token string) (*Result, error) {
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
	if s.closed {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	credential := credentialKey(selected)
	if resume {
		if s.blockedCredentials[credential] == "" || s.blockedCredentials[credential] != token {
			s.mu.Unlock()
			return nil, ErrConflict
		}
		delete(s.blockedCredentials, credential)
		s.mu.Unlock()
		return &Result{}, nil
	}
	generation := randomID()
	if generation == "" {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	s.blockedCredentials[credential] = generation
	var waits []<-chan struct{}
	for _, run := range s.backfillActive {
		if credentialKey(run.target) == credential {
			run.cancel()
			waits = append(waits, run.done)
		}
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
	s.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ErrConflict
		}
	}
	return &Result{CredentialChangeToken: generation}, nil
}
