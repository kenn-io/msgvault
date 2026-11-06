package emailpush

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/oauth"
)

var ErrSourceUnavailable = errors.New("gmail push source is unavailable")

const gmailListenerCancellation = "gmail listener cancellation"

// GmailSubscriber is an exclusive, account-bound subscription transport. The
// caller establishes exclusive ownership before Run; Validate verifies provider
// configuration. No other account or consumer may share this subscription.
type GmailSubscriber interface {
	Validate(ctx context.Context) error
	Pull(ctx context.Context) ([]Delivery, error)
	Acknowledge(ctx context.Context, ids []string) error
}

// GmailWatch is returned only after strict source-bound profile verification and
// successful mailbox-wide watch setup. History tokens are intentionally absent.
type GmailWatch struct {
	EmailAddress string
	Expiration   time.Time
}

// GmailListenerClock permits deterministic renewal and retry tests. Implementations
// must support concurrent calls from the subscriber and renewal loops.
type GmailListenerClock interface {
	Now() time.Time
	After(delay time.Duration) <-chan time.Time
}

type gmailRealClock struct{}

func (gmailRealClock) Now() time.Time                         { return time.Now() }
func (gmailRealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// GmailListener connects provider hints to an exact-source reconciliation receipt.
// Callbacks must honor context cancellation. Watch must verify the authenticated
// profile against the immutable source before installing or renewing the watch;
// best-effort identity checks do not satisfy this contract. Reconcile returns nil
// only after complete committed catchup (no partial acquisition or preemption).
// ValidateSource fences retirement/replacement before work and before each ACK.
//
// Do not mutate fields while Run is active. OnError, when supplied, must be prompt
// and concurrency-safe; its errors redact provider bodies and account identifiers.
// Initial setup fails closed. After setup, retries use bounded exponential jitter;
// polling remains the fallback throughout failures and finite broker retention.
type GmailListener struct {
	Subscriber         GmailSubscriber
	Watch              func(context.Context) (GmailWatch, error)
	ValidateSource     func(context.Context) error
	Reconcile          func(context.Context) error
	OnError            func(error)
	Clock              GmailListenerClock
	RetryMin, RetryMax time.Duration
}

type gmailListenerError struct {
	operation string
	cause     error
}

func (e *gmailListenerError) Error() string { return e.operation + " failed" }
func (e *gmailListenerError) Unwrap() error { return e.cause }

type gmailSourceError struct{ cause error }

func (e *gmailSourceError) Error() string        { return ErrSourceUnavailable.Error() }
func (e *gmailSourceError) Unwrap() error        { return e.cause }
func (e *gmailSourceError) Is(target error) bool { return target == ErrSourceUnavailable }

type gmailListenerRun struct {
	*GmailListener

	clock              GmailListenerClock
	retryMin, retryMax time.Duration
}

// Run blocks until cancellation, source unavailability or terminal setup failure.
// Renewal runs independently even while reconciliation waits for the archive gate.
// On return all listener goroutines are joined. Successful empty Pulls never imply
// reconnect; only recovery from a failed Pull requests another catchup.
func (l *GmailListener) Run(parent context.Context) error {
	if l.Subscriber == nil || l.Watch == nil || l.ValidateSource == nil || l.Reconcile == nil {
		return errors.New("gmail listener requires subscription, verified watch, source and reconciliation callbacks")
	}
	s := &gmailListenerRun{GmailListener: l, clock: l.Clock, retryMin: l.RetryMin, retryMax: l.RetryMax}
	if s.clock == nil {
		s.clock = gmailRealClock{}
	}
	if s.retryMin == 0 {
		s.retryMin = time.Second
	}
	if s.retryMax == 0 {
		s.retryMax = max(time.Minute, s.retryMin)
	}
	if s.retryMin < 0 || s.retryMax < s.retryMin {
		return errors.New("invalid Gmail listener retry bounds")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	if err := s.checkSource(parent); err != nil {
		return err
	}
	if err := s.Subscriber.Validate(parent); err != nil {
		return &gmailListenerError{operation: "Gmail subscription validation", cause: err}
	}
	if err := s.checkSource(parent); err != nil {
		return err
	}
	initial, err := s.Watch(parent)
	if err != nil {
		return &gmailListenerError{operation: "Gmail watch setup", cause: err}
	}
	if !bareEmail(initial.EmailAddress) || !initial.Expiration.After(s.clock.Now()) {
		return errors.New("gmail watch setup returned invalid identity or expiration")
	}
	ctx, cancel := context.WithCancelCause(parent)
	renewed := make(chan struct{})
	renewHints := make(chan struct{}, 1)
	go func() { defer close(renewed); cancel(s.renew(ctx, initial, renewHints)) }()
	defer func() { cancel(context.Canceled); <-renewed }()
	// The subscriber owns all catchup. Startup debt exists only after strict setup.
	err = s.pull(ctx, initial.EmailAddress, renewHints)
	return listenerCause(ctx, err)
}

func (s *gmailListenerRun) checkSource(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ValidateSource(ctx); err != nil {
		if errors.Is(err, ErrSourceUnavailable) {
			return &gmailSourceError{cause: err}
		}
		return &gmailListenerError{operation: "Gmail source validation", cause: err}
	}
	return nil
}
func (s *gmailListenerRun) catchup(ctx context.Context) error {
	if err := s.checkSource(ctx); err != nil {
		return err
	}
	if err := s.Reconcile(ctx); err != nil {
		return &gmailListenerError{operation: "Gmail reconciliation", cause: err}
	}
	return nil
}
func (s *gmailListenerRun) ack(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := s.checkSource(ctx); err != nil {
		return err
	}
	if err := s.Subscriber.Acknowledge(ctx, ids); err != nil {
		return &gmailListenerError{operation: "Gmail notification acknowledgement", cause: err}
	}
	return nil
}

func (s *gmailListenerRun) pull(ctx context.Context, email string, renewHints <-chan struct{}) error {
	delay := s.retryMin
	poisonDelay := s.retryMin
	idleDelay := s.retryMin
	idleCap := max(s.retryMin, min(s.retryMax, 10*time.Second))
	owed := true // Startup, renewal and recovery share one retained catchup intent.
	recovering, revalidate := false, false
	var pendingACK []string // At most one delivered batch, retained until successful ACK.
	for {
		if err := s.checkSource(ctx); err != nil {
			if errors.Is(err, ErrSourceUnavailable) || ctx.Err() != nil {
				return err
			}
			s.report(err)
			if !s.wait(ctx, s.jitter(delay)) {
				return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
			}
			delay = s.nextDelay(delay)
			continue
		}
		select {
		case <-renewHints:
			owed = true
			idleDelay = s.retryMin
		default:
		}
		completed := false
		if owed {
			if err := s.catchup(ctx); err != nil {
				if errors.Is(err, ErrSourceUnavailable) || ctx.Err() != nil {
					return err
				}
				s.report(err)
				if !s.wait(ctx, s.jitter(delay)) {
					return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
				}
				delay = s.nextDelay(delay)
				continue
			}
			// A renewal arriving during this pass remains in renewHints for a follow-up.
			owed = false
			completed = true
			delay = s.retryMin
		}
		if len(pendingACK) > 0 {
			if err := s.ack(ctx, pendingACK); err != nil {
				if errors.Is(err, ErrSourceUnavailable) || ctx.Err() != nil || terminalSubscriberError(err) {
					return err
				}
				s.report(err)
				if !s.wait(ctx, s.jitter(delay)) {
					return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
				}
				delay = s.nextDelay(delay)
				continue
			}
			pendingACK = nil
			delay = s.retryMin
		}
		if completed {
			continue
		}
		if revalidate {
			if err := s.Subscriber.Validate(ctx); err != nil {
				if ctx.Err() != nil || terminalSubscriberError(err) {
					return &gmailListenerError{operation: "Gmail subscription validation", cause: err}
				}
				s.report(&gmailListenerError{operation: "Gmail subscription validation", cause: err})
				if !s.wait(ctx, s.jitter(delay)) {
					return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
				}
				delay = s.nextDelay(delay)
				continue
			}
			revalidate = false
		}
		deliveries, wake, err := s.pullOrRenew(ctx, renewHints)
		if wake {
			owed = true
			idleDelay = s.retryMin
			continue
		}
		if err != nil {
			if ctx.Err() != nil || terminalSubscriberError(err) {
				return &gmailListenerError{operation: "Gmail notification Pull", cause: err}
			}
			s.report(&gmailListenerError{operation: "Gmail notification Pull", cause: err})
			recovering, revalidate = true, true
			if !s.wait(ctx, s.jitter(delay)) {
				return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
			}
			delay = s.nextDelay(delay)
			continue
		}
		delay = s.retryMin
		if recovering {
			owed = true
			recovering = false
			idleDelay = s.retryMin
		}
		if len(deliveries) > 0 {
			idleDelay = s.retryMin
		}
		if len(deliveries) > MaxPullMessages {
			return errors.New("gmail notification batch exceeds limit")
		}
		var poison []string
		for _, delivery := range deliveries {
			if delivery.AckID == "" || len(delivery.AckID) > MaxAckIDBytes {
				return errors.New("invalid Gmail notification ACK token")
			}
			if _, err := ParseNotification(delivery.Data, email); err != nil {
				poison = append(poison, delivery.AckID)
			} else {
				pendingACK = append(pendingACK, delivery.AckID)
			}
		}
		if len(pendingACK) > 0 {
			owed = true
		}
		if err := s.ack(ctx, poison); err != nil {
			if errors.Is(err, ErrSourceUnavailable) || ctx.Err() != nil || terminalSubscriberError(err) {
				return err
			}
			s.report(err)
			// Poison debt remains at the broker; it must not block valid acquisition.
			if !owed && !s.wait(ctx, s.jitter(poisonDelay)) {
				return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
			}
			poisonDelay = s.nextDelay(poisonDelay)
		} else if len(poison) > 0 {
			poisonDelay = s.retryMin
		}
		if len(deliveries) == 0 && !owed {
			// Quiet empty Pulls back off without inventing catchup. Renewal and
			// cancellation interrupt the wait independently of idle pacing.
			select {
			case <-ctx.Done():
				return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
			case <-renewHints:
				owed = true
				idleDelay = s.retryMin
			case <-s.clock.After(idleDelay):
				idleDelay = min(idleCap, s.nextDelay(idleDelay))
			}
		}
	}
}

type gmailPullResult struct {
	deliveries []Delivery
	err        error
}

// At most one Pull goroutine exists. Internal cancellation services a renewal
// hint without creating a false transport-recovery hint. A batch that races
// cancellation remains unacknowledged and therefore eligible for redelivery.
func (s *gmailListenerRun) pullOrRenew(ctx context.Context, hints <-chan struct{}) ([]Delivery, bool, error) {
	pullCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan gmailPullResult, 1)
	go func() {
		deliveries, err := s.Subscriber.Pull(pullCtx)
		done <- gmailPullResult{deliveries: deliveries, err: err}
	}()
	select {
	case result := <-done:
		return result.deliveries, false, result.err
	case <-hints:
		cancel()
		<-done
		return nil, true, nil
	case <-ctx.Done():
		cancel()
		<-done
		return nil, false, &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
	}
}

func (s *gmailListenerRun) renew(ctx context.Context, initial GmailWatch, hints chan<- struct{}) error {
	expiry := initial.Expiration
	delay := min(24*time.Hour, expiry.Sub(s.clock.Now())/2)
	retry := s.retryMin
	for {
		if !s.wait(ctx, delay) {
			return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
		}
		if err := s.checkSource(ctx); err != nil {
			if errors.Is(err, ErrSourceUnavailable) || ctx.Err() != nil {
				return err
			}
			s.report(err)
			delay = s.jitter(retry)
			retry = s.nextDelay(retry)
			continue
		}
		watch, err := s.Watch(ctx)
		if err != nil {
			if errors.Is(err, ErrSourceUnavailable) || ctx.Err() != nil {
				return &gmailListenerError{operation: "Gmail watch renewal", cause: err}
			}
			s.report(&gmailListenerError{operation: "Gmail watch renewal", cause: err})
			delay = s.jitter(retry)
			retry = s.nextDelay(retry)
			continue
		}
		if !bareEmail(watch.EmailAddress) || !oauth.SameGoogleAccount(initial.EmailAddress, watch.EmailAddress) {
			return &gmailSourceError{cause: errors.New("renewed Gmail profile no longer matches")}
		}
		if !watch.Expiration.After(s.clock.Now()) {
			s.report(errors.New("gmail watch renewal returned invalid expiration"))
			delay = s.jitter(retry)
			retry = s.nextDelay(retry)
			continue
		}
		select {
		case hints <- struct{}{}:
		default:
		}
		expiry = watch.Expiration
		retry = s.retryMin
		delay = min(24*time.Hour, expiry.Sub(s.clock.Now())/2)
	}
}

func (s *gmailListenerRun) report(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}
func (s *gmailListenerRun) wait(ctx context.Context, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.clock.After(delay):
		return ctx.Err() == nil
	}
}
func (s *gmailListenerRun) nextDelay(delay time.Duration) time.Duration {
	if delay > s.retryMax/2 {
		return s.retryMax
	}
	return min(s.retryMax, delay*2)
}
func (s *gmailListenerRun) jitter(delay time.Duration) time.Duration {
	lower := max(time.Nanosecond, delay/2)
	return lower + time.Duration(rand.Int64N(int64(delay-lower)+1)) //nolint:gosec // Retry timing jitter does not generate secrets.
}
func terminalSubscriberError(err error) bool {
	if _, invalid := errors.AsType[*ValidationError](err); invalid {
		return true
	}
	status, ok := errors.AsType[*HTTPError](err)
	return ok && status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout && status.StatusCode != http.StatusTooManyRequests
}

func listenerCause(ctx context.Context, fallback error) error {
	if ctx.Err() != nil {
		return &gmailListenerError{operation: gmailListenerCancellation, cause: context.Cause(ctx)}
	}
	return fallback
}
