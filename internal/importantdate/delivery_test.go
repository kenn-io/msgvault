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
	claimed               bool
	state                 store.ImportantDateDeliveryState
	consent               bool
	resolveBeforeDispatch bool
	issueUID              string
}

func (f *deliveryStoreFake) ClaimImportantDateDeliveryContext(context.Context, string, string) (bool, error) {
	if f.claimed && f.state != store.ImportantDateRetryIssue {
		return false, nil
	}
	f.claimed = true
	f.state = store.ImportantDateClaimed
	return true, nil
}
func (f *deliveryStoreFake) ClaimImportantDateNotificationContext(_ context.Context, _ string, _ string, issue string) (bool, error) {
	if f.state != store.ImportantDateIssueCreated || f.issueUID != issue {
		return false, nil
	}
	f.state = store.ImportantDateNotifyInFlight
	return true, nil
}
func (f *deliveryStoreFake) AdvanceImportantDateDeliveryContext(_ context.Context, _ string, from, to store.ImportantDateDeliveryState, issue string) (bool, error) {
	if f.state != from {
		return false, nil
	}
	f.state = to
	f.issueUID = issue
	return true, nil
}
func (f *deliveryStoreFake) ImportantDateDeliveryContext(context.Context, string) (store.ImportantDateDeliveryState, string, error) {
	return f.state, f.issueUID, nil
}
func (f *deliveryStoreFake) WithImportantDateConsentContext(ctx context.Context, _, _, _, _ string, dispatch func(context.Context) error) (bool, error) {
	if !f.consent {
		return false, nil
	}
	if f.resolveBeforeDispatch {
		f.state = store.ImportantDateCancelled
	}
	return true, dispatch(ctx)
}

type deliveryKataFake struct {
	projectUID             string
	lookupError            error
	existing               bool
	notifyError            error
	issueStatus            string
	creates, notifications int
}

func (f *deliveryKataFake) ProjectUID(context.Context, string) (string, error) {
	if f.projectUID != "" {
		return f.projectUID, nil
	}
	return "project-example", nil
}
func (f *deliveryKataFake) FindActionTask(context.Context, string, string) (taskclient.KataTask, bool, error) {
	if f.lookupError != nil {
		return taskclient.KataTask{}, false, f.lookupError
	}
	if f.existing {
		return taskclient.KataTask{UID: "issue-example", Project: "private-example", Status: "open"}, true, nil
	}
	return taskclient.KataTask{}, false, nil
}
func (f *deliveryKataFake) CreateTaskReused(_ context.Context, project, _ string, _ taskclient.KataCreate) (taskclient.KataTask, bool, error) {
	f.creates++
	return taskclient.KataTask{UID: "issue-example", Project: project, Status: "open"}, false, nil
}
func (f *deliveryKataFake) GetTask(_ context.Context, project, uid string) (taskclient.KataTask, error) {
	status := f.issueStatus
	if status == "" {
		status = "open"
	}
	return taskclient.KataTask{UID: uid, Project: project, Status: status}, nil
}
func (f *deliveryKataFake) NotifyIssue(context.Context, string, string, string, string) error {
	f.notifications++
	return f.notifyError
}

func TestDeliveryDoesNotRetryUncertainNotification(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{notifyError: errors.New("response lost")}
	service := DeliveryService{Store: st, Kata: kata}
	input := DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}
	err := service.Deliver(t.Context(), input, func(context.Context) (bool, error) { return true, nil })
	must.Error(err)
	checks.Equal(store.ImportantDateUnknown, st.state)
	checks.Equal(1, kata.notifications)
	err = service.Deliver(t.Context(), input, func(context.Context) (bool, error) { return true, nil })
	must.NoError(err)
	checks.Equal(1, kata.notifications)
}

func TestDeliveryDoesNotWriteWithoutConsent(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{}
	kata := &deliveryKataFake{}
	service := DeliveryService{Store: st, Kata: kata}
	err := service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	must.NoError(err)
	checks.Zero(kata.creates)
	checks.Zero(kata.notifications)
	checks.Equal(store.ImportantDateRetryIssue, st.state)
}

func TestDeliveryDoesNotRenotifyExistingIssue(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{existing: true}
	service := DeliveryService{Store: st, Kata: kata}
	err := service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	must.NoError(err)
	checks.Equal(store.ImportantDateCancelled, st.state)
	checks.Zero(kata.notifications)
}

func TestDeliveryStopsIfOperatorResolvedBeforeDispatch(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{consent: true, resolveBeforeDispatch: true}
	kata := &deliveryKataFake{}
	service := DeliveryService{Store: st, Kata: kata}
	err := service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	must.NoError(err)
	checks.Zero(kata.creates)
	checks.Zero(kata.notifications)
}

func TestDeliveryCancelsDefiniteLookupFailure(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{lookupError: errors.New("lookup unavailable")}
	service := DeliveryService{Store: st, Kata: kata}
	err := service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	must.Error(err)
	checks.Equal(store.ImportantDateRetryIssue, st.state)
	checks.Zero(kata.creates)
	checks.Zero(kata.notifications)
	kata.lookupError = nil
	err = service.Deliver(t.Context(), DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}, func(context.Context) (bool, error) { return true, nil })
	must.NoError(err)
	checks.Equal(store.ImportantDateDelivered, st.state)
	checks.Equal(1, kata.notifications)
}

func TestDeliveryRetriesNotificationPreflightWithoutRecreatingIssue(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{}
	service := DeliveryService{Store: st, Kata: kata}
	input := DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}
	validations := 0
	revalidate := func(context.Context) (bool, error) {
		validations++
		if validations == 2 {
			return false, errors.New("preflight unavailable")
		}
		return true, nil
	}
	err := service.Deliver(t.Context(), input, revalidate)
	must.Error(err)
	checks.Equal(store.ImportantDateIssueCreated, st.state)
	checks.Equal(1, kata.creates)
	checks.Zero(kata.notifications)
	err = service.Deliver(t.Context(), input, revalidate)
	must.NoError(err)
	checks.Equal(store.ImportantDateDelivered, st.state)
	checks.Equal(1, kata.creates)
	checks.Equal(1, kata.notifications)
}

func TestDeliveryDoesNotNotifyClosedIssueAfterPreflightFailure(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := &deliveryStoreFake{consent: true}
	kata := &deliveryKataFake{}
	service := DeliveryService{Store: st, Kata: kata}
	input := DeliveryInput{OccurrenceKey: "occurrence-example", Fingerprint: "fingerprint-example", SubjectUID: "person/subject-example", OwnerUID: "person/owner-example", Project: "private-example", ConsentScope: "scope-example", Recipient: "owner/agent"}
	calls := 0
	revalidate := func(context.Context) (bool, error) {
		calls++
		if calls == 2 {
			return false, errors.New("preflight unavailable")
		}
		return true, nil
	}
	must.Error(service.Deliver(t.Context(), input, revalidate))
	kata.issueStatus = "closed"
	must.NoError(service.Deliver(t.Context(), input, revalidate))
	checks.Equal(store.ImportantDateCancelled, st.state)
	checks.Equal(1, kata.creates)
	checks.Zero(kata.notifications)
}
