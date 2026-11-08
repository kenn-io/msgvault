package importantdate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

type deliveryStoreFake struct {
	claimed bool
	state   store.ImportantDateDeliveryState
	consent bool
}

func (f *deliveryStoreFake) ClaimImportantDateDeliveryContext(context.Context, string) (bool, error) {
	if f.claimed {
		return false, nil
	}
	f.claimed = true
	f.state = store.ImportantDateClaimed
	return true, nil
}
func (f *deliveryStoreFake) AdvanceImportantDateDeliveryContext(_ context.Context, _ string, from, to store.ImportantDateDeliveryState, _ string) (bool, error) {
	if f.state != from {
		return false, nil
	}
	f.state = to
	return true, nil
}
func (f *deliveryStoreFake) WithImportantDateConsentContext(ctx context.Context, _, _, _ string, dispatch func(context.Context) error) (bool, error) {
	if !f.consent {
		return false, nil
	}
	return true, dispatch(ctx)
}

type deliveryKataFake struct {
	existing               bool
	notifyError            error
	creates, notifications int
}

func (f *deliveryKataFake) FindActionTask(context.Context, string, string) (taskclient.KataTask, bool, error) {
	if f.existing {
		return taskclient.KataTask{UID: "issue-example", Project: "private-example", Status: "open"}, true, nil
	}
	return taskclient.KataTask{}, false, nil
}
func (f *deliveryKataFake) CreateTaskReused(_ context.Context, project, _ string, _ taskclient.KataCreate) (taskclient.KataTask, bool, error) {
	f.creates++
	return taskclient.KataTask{UID: "issue-example", Project: project, Status: "open"}, false, nil
}
func (f *deliveryKataFake) NotifyIssue(context.Context, string, string, string, string) error {
	f.notifications++
	return f.notifyError
}

func TestDeliveryDoesNotRetryUncertainNotification(t *testing.T) {
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{notifyError: errors.New("response lost")}
	service := DeliveryService{Store: st, Kata: kata}
	input := DeliveryInput{OccurrenceKey: "occurrence-example", SubjectUID: "subject-example", Project: "private-example", Recipient: "owner/agent"}
	err := service.Deliver(t.Context(), input, func(context.Context) (bool, error) { return true, nil })
	require.Error(t, err)
	assert.Equal(t, store.ImportantDateUnknown, st.state)
	assert.Equal(t, 1, kata.notifications)
	err = service.Deliver(t.Context(), input, func(context.Context) (bool, error) { return true, nil })
	require.NoError(t, err)
	assert.Equal(t, 1, kata.notifications)
}

func TestDeliveryDoesNotWriteWithoutConsent(t *testing.T) {
	st := &deliveryStoreFake{}
	kata := &deliveryKataFake{}
	service := DeliveryService{Store: st, Kata: kata}
	err := service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", SubjectUID: "subject-example", Project: "private-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	require.NoError(t, err)
	assert.Zero(t, kata.creates)
	assert.Zero(t, kata.notifications)
}

func TestDeliveryDoesNotRenotifyExistingIssue(t *testing.T) {
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{existing: true}
	service := DeliveryService{Store: st, Kata: kata}
	err := service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", SubjectUID: "subject-example", Project: "private-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	require.NoError(t, err)
	assert.Equal(t, store.ImportantDateCancelled, st.state)
	assert.Zero(t, kata.notifications)
}
