package loops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func (d *Deriver) cutoff(now time.Time) time.Time {
	if d.cfg.Lookback == 0 {
		return time.Time{}
	}
	return now.Add(-d.cfg.Lookback)
}
func (d *Deriver) epoch(ctx context.Context) (store.LoopEpoch, error) {
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%d:%d:%d:%d", d.cfg.ReplyOwedAfter, d.cfg.AwaitingReplyAfter, d.cfg.MaxGroupCounterparts, d.cfg.Lookback)))
	return d.st.CurrentLoopEpochContext(ctx, RulesetVersion, fingerprint)
}
func (d *Deriver) prepareRescan(ctx context.Context, now time.Time) (store.LoopEpoch, error) {
	epoch, err := d.epoch(ctx)
	if err != nil {
		return epoch, err
	}
	state, err := d.State(ctx)
	if err != nil {
		return epoch, err
	}
	if state.RescanReason != "" && state.RescanEpoch == epoch.String() {
		return epoch, nil
	}
	reason := ""
	switch {
	case state.IdentityRevision != epoch.IdentityRevision || state.AccountIdentityRevision != epoch.AccountIdentityRevision:
		reason = "identity"
	case state.RulesetVersion != epoch.RulesetVersion:
		reason = "ruleset"
	case state.ConfigFingerprint != epoch.ConfigFingerprint:
		reason = "config"
	case state.RescanReason != "":
		reason = state.RescanReason
	case state.LastReconcileAt == nil || now.Sub(*state.LastReconcileAt) >= d.cfg.ReconcileInterval:
		reason = "reconcile"
	}
	if reason != "" {
		err = d.st.BeginLoopRescanContext(ctx, epoch, reason, nil, now)
	}
	return epoch, err
}
func (d *Deriver) Rebuild(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().UTC()
	epoch, err := d.epoch(ctx)
	if err != nil {
		return err
	}
	start := d.cutoff(now)
	return d.st.BeginLoopRescanContext(ctx, epoch, "rebuild", &start, now)
}
