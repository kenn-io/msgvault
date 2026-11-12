package storetest

import (
	"strconv"
	"time"
)

// PersistLoopOriginalReadFixture creates a current question and older acknowledgements.
func PersistLoopOriginalReadFixture(f *Fixture, acknowledgements int, now time.Time) {
	f.PersistEmail(EmailSpec{Thread: "question", Subject: "Can you send the contract?", From: "alex@example.test", To: []string{OwnerAddress}, Body: "Can you send the contract?", SentAt: now.Add(-2 * 24 * time.Hour)})
	for i := range acknowledgements {
		f.PersistEmail(EmailSpec{Thread: "ack-" + strconv.Itoa(i), From: "alex@example.test", To: []string{OwnerAddress}, Body: "Thanks!", SentAt: now.Add(-3 * 24 * time.Hour)})
	}
}
