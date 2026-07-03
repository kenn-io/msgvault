package cmd

import (
	"context"
	"slices"
	"sync"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/scheduler"
)

// labelWorkTracker wraps an operation gate so acquisitions from this caller
// identify themselves to waiters (the busy response names the holder).
func labelWorkTracker(gate api.LabeledOperationGate, label string) scheduler.WorkTracker {
	if gate == nil {
		return nil
	}
	return labeledWorkTracker{gate: gate, label: label}
}

type labeledWorkTracker struct {
	gate  api.LabeledOperationGate
	label string
}

func (t labeledWorkTracker) BeginWork() (func(), bool) {
	return t.BeginWorkContext(context.Background())
}

func (t labeledWorkTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	return t.gate.BeginLabeledWorkContext(ctx, t.label)
}

func combineWorkTrackers(trackers ...scheduler.WorkTracker) scheduler.WorkTracker {
	filtered := make([]scheduler.WorkTracker, 0, len(trackers))
	for _, tracker := range trackers {
		if tracker != nil {
			filtered = append(filtered, tracker)
		}
	}
	if len(filtered) == 0 {
		return noopWorkTracker{}
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return combinedWorkTracker{trackers: filtered}
}

type noopWorkTracker struct{}

func (noopWorkTracker) BeginWork() (func(), bool) {
	return func() {}, true
}

func (noopWorkTracker) BeginWorkContext(context.Context) (func(), bool) {
	return func() {}, true
}

type combinedWorkTracker struct {
	trackers []scheduler.WorkTracker
}

func (t combinedWorkTracker) BeginWork() (func(), bool) {
	return t.BeginWorkContext(context.Background())
}

func (t combinedWorkTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	dones := make([]func(), 0, len(t.trackers))
	for _, tracker := range t.trackers {
		if ctx.Err() != nil {
			for _, v := range slices.Backward(dones) {
				v()
			}
			return func() {}, false
		}
		done, ok := tracker.BeginWorkContext(ctx)
		if !ok {
			for _, v := range slices.Backward(dones) {
				v()
			}
			return func() {}, false
		}
		dones = append(dones, done)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			for _, v := range slices.Backward(dones) {
				v()
			}
		})
	}, true
}
