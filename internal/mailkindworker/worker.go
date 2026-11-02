// Package mailkindworker runs explicitly requested, consented mail-kind batches.
package mailkindworker

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/store"
)

var ErrConsentRequired = errors.New("mail kind consent is required")

type Store interface {
	HasMailKindConsentContext(ctx context.Context, fingerprint string) (bool, error)
	MailKindConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error)
	ListMailKindCandidatesContext(ctx context.Context, limit int, ids []int64) ([]int64, error)
	NextMailKindCandidatesContext(ctx context.Context, fingerprint string, limit int) ([]int64, error)
	MailKindInputContext(ctx context.Context, id int64) (mailkind.Input, string, error)
	ApplyMailKindLabelContext(ctx context.Context, fingerprint string, id int64, token, kind string, mutationGate func(context.Context) (func(), error)) (bool, error)
}

type Worker struct {
	Store         Store
	Config        mailkind.Config
	Endpoint      string // Empty uses the pinned endpoint; tests use a local fixture.
	HTTPClient    *http.Client
	BeginMutation func(context.Context) (func(), error)
}

// Counts deliberately exposes no message content, IDs or provider prose.
type Counts struct {
	Classified int `json:"classified"`
	Labeled    int `json:"labeled"`
	Undecided  int `json:"undecided"`
	Errors     int `json:"errors"`
	Requests   int `json:"requests"`
}

// Run preflights exact-purpose consent before loading any candidate body.
// Each actual request, retry and final label write checks it again.
func (w Worker) Run(ctx context.Context, limit int, messageIDs []int64) (Counts, error) {
	counts := Counts{}
	if w.Store == nil {
		return counts, errors.New("mail kind store is unavailable")
	}
	cfg := w.Config
	cfg.ApplyDefaults()
	disclosure, err := cfg.Disclosure()
	if err != nil {
		return counts, err
	}
	fingerprint, err := disclosure.Fingerprint()
	if err != nil {
		return counts, err
	}
	if limit < 1 || limit > cfg.BatchSize || len(messageIDs) > limit {
		return counts, errors.New("mail kind limit exceeds configured batch size")
	}
	active, err := w.Store.HasMailKindConsentContext(ctx, fingerprint)
	if err != nil {
		return counts, err
	}
	if !active {
		return counts, ErrConsentRequired
	}
	key, err := cfg.CredentialFromEnvironment()
	if err != nil {
		return counts, err
	}
	var ids []int64
	if len(messageIDs) > 0 {
		ids, err = w.Store.ListMailKindCandidatesContext(ctx, limit, messageIDs)
	} else {
		ids, err = w.Store.NextMailKindCandidatesContext(ctx, fingerprint, limit)
	}
	if err != nil {
		return counts, err
	}
	endpoint := w.Endpoint
	if endpoint == "" {
		endpoint = mailkind.Endpoint
	}
	var decider mailkind.Decider = mailkind.JevClient{Endpoint: endpoint, Key: key, HTTPClient: w.HTTPClient,
		RequestGate: func(dispatchContext context.Context, dispatch func() error) (bool, error) {
			return w.Store.MailKindConsentEgressContext(dispatchContext, fingerprint, dispatch)
		}}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		active, err := w.Store.HasMailKindConsentContext(ctx, fingerprint)
		if err != nil {
			return counts, err
		}
		if !active {
			return counts, ErrConsentRequired
		}
		input, token, err := w.Store.MailKindInputContext(ctx, id)
		if errors.Is(err, store.ErrMailKindIneligible) {
			counts.Undecided++
			continue
		}
		if err != nil {
			counts.Errors++
			continue
		}
		packet, eligible := mailkind.BuildPacket(input)
		if !eligible {
			counts.Undecided++
			continue
		}
		judgment, err := decider.Decide(ctx, packet)
		counts.Requests += judgment.Requests
		if errors.Is(err, mailkind.ErrConsentRevoked) {
			return counts, ErrConsentRequired
		}
		if err != nil {
			counts.Errors++
			continue
		}
		counts.Classified++
		if judgment.Transactional < cfg.MinimumTransactional || judgment.Probability < cfg.MinimumChoiceProbability {
			counts.Undecided++
			continue
		}
		changed, err := w.Store.ApplyMailKindLabelContext(ctx, fingerprint, id, token, judgment.Kind, w.BeginMutation)
		if errors.Is(err, mailkind.ErrConsentRevoked) {
			return counts, ErrConsentRequired
		}
		if errors.Is(err, store.ErrMailKindStale) || errors.Is(err, store.ErrMailKindIneligible) {
			counts.Undecided++
			continue
		}
		if err != nil {
			return counts, err
		}
		if changed {
			counts.Labeled++
		}
	}
	return counts, nil
}
