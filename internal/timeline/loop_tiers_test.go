package timeline

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestTierFilterBeforeLoopLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	person := f.savePerson()
	f.tier(person, 1)
	for i := range 11 {
		f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("older-%d", i), From: fmt.Sprintf("peer%d@example.com", i), To: []string{storetest.OwnerAddress}, Body: "Can you send the invoice?", SentAt: f.now.Add(-72 * time.Hour)})
	}
	f.PersistEmail(storetest.EmailSpec{Thread: "selected", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Body: "Can you send the contract?", SentAt: f.now.Add(-48 * time.Hour)})
	f.project()
	req := dayRequest()
	req.Tiers = []int{1}
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.OpenLoops)
	requirements.Len(got.OpenLoops.Items, 1)
	assertions.Equal(person, got.OpenLoops.Items[0].Loop.Person.PersonID)
}
