package storetest

import (
	"context"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// LoopOriginalReadStore counts real original reads against an archive fixture.
type LoopOriginalReadStore struct {
	*store.Store

	OriginalReads int
}

// NewLoopOriginalReadStore creates a current question and older acknowledgements.
func NewLoopOriginalReadStore(f *Fixture, acknowledgements int, now time.Time) *LoopOriginalReadStore {
	f.PersistEmail(EmailSpec{Thread: "question", Subject: "Can you send the contract?", From: "alex@example.test", To: []string{OwnerAddress}, Body: "Can you send the contract?", SentAt: now.Add(-2 * 24 * time.Hour)})
	for i := range acknowledgements {
		f.PersistEmail(EmailSpec{Thread: "ack-" + strconv.Itoa(i), From: "alex@example.test", To: []string{OwnerAddress}, Body: "Thanks!", SentAt: now.Add(-3 * 24 * time.Hour)})
	}
	return &LoopOriginalReadStore{Store: f.Store}
}

func (s *LoopOriginalReadStore) LoopOriginalEvidenceContext(ctx context.Context, id int64) (mime.AutomationSignals, bool, bool, error) {
	s.OriginalReads++
	return s.Store.LoopOriginalEvidenceContext(ctx, id)
}
