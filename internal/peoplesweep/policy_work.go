package peoplesweep

import "context"

// The archive supplies policy authority; package fixtures may omit it.
type personSweepPolicyStore interface {
	CapturePersonSweepPolicyWork(ctx context.Context, personID int64) (context.Context, error)
	BindPersonSweepPolicyInputs(ctx context.Context, items []EvidenceItem) (context.Context, error)
	CheckPersonSweepPolicyWork(ctx context.Context) error
}

func (w *Worker) capturePolicyWork(ctx context.Context, personID int64) (context.Context, error) {
	if policy, ok := w.Store.(personSweepPolicyStore); ok {
		return policy.CapturePersonSweepPolicyWork(ctx, personID)
	}
	return ctx, nil
}
func (w *Worker) bindPolicyInputs(ctx context.Context, batches []PacketBatch) (context.Context, error) {
	policy, ok := w.Store.(personSweepPolicyStore)
	if !ok {
		return ctx, nil
	}
	var inputs []EvidenceItem
	for _, batch := range batches {
		inputs = append(inputs, batch.Packet.Seeds...)
		inputs = append(inputs, batch.Packet.Context...)
	}
	return policy.BindPersonSweepPolicyInputs(ctx, inputs)
}
func (w *Worker) checkPolicyWork(ctx context.Context) error {
	if policy, ok := w.Store.(personSweepPolicyStore); ok {
		return policy.CheckPersonSweepPolicyWork(ctx)
	}
	return nil
}
