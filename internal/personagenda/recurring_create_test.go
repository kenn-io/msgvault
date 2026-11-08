package personagenda

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

type fakeRecurringCreateClient struct {
	TaskClient

	found      bool
	parent     taskclient.KataTask
	job        taskclient.KataCronJob
	loseJob    bool
	creates    int
	catalogErr error
}

func (c *fakeRecurringCreateClient) CheckRecurrenceSupport(context.Context, string) error {
	return c.catalogErr
}

func (c *fakeRecurringCreateClient) FindActionTask(_ context.Context, _ string, marker string) (taskclient.KataTask, bool, error) {
	return c.parent, c.found && c.parent.Metadata[taskclient.ActionMetadataKey] == marker, nil
}
func (c *fakeRecurringCreateClient) GetCronJob(_ context.Context, _ string, jobID string) (taskclient.KataCronJob, error) {
	if c.job.UID != "" && c.job.UID == jobID {
		return c.job, nil
	}
	return taskclient.KataCronJob{}, taskclient.ErrNotFound
}
func (c *fakeRecurringCreateClient) CreateCronJob(_ context.Context, _ string, jobID, parentID string, settings taskclient.KataCronSettings) (taskclient.KataCronJob, error) {
	c.creates++
	c.job = taskclient.KataCronJob{UID: jobID, ParentUID: parentID, DefinitionEventUID: fixtureEventUID, TriggerKind: settings.TriggerKind, Cron: settings.Cron, IntervalSeconds: settings.IntervalSeconds, Timezone: settings.Timezone, OccurrenceTitle: settings.OccurrenceTitle, OccurrenceBody: settings.OccurrenceBody, Recipient: settings.Recipient, Catchup: settings.Catchup, GraceSeconds: settings.GraceSeconds, Enabled: true}
	if c.loseJob {
		c.loseJob = false
		return taskclient.KataCronJob{}, taskclient.ErrUnreachable
	}
	return c.job, nil
}
func (c *fakeRecurringCreateClient) CreateTask(_ context.Context, _ string, _ string, create taskclient.KataCreate) (taskclient.KataTask, error) {
	c.creates++
	c.found = true
	c.parent = taskclient.KataTask{UID: fixtureParentUID, ProjectID: 1, Owner: create.Owner, Title: create.Title, Status: "open", Metadata: create.Metadata}
	return c.parent, nil
}
func (c *fakeRecurringCreateClient) GetTask(context.Context, string, string) (taskclient.KataTask, error) {
	return c.parent, nil
}

func TestRecurringCreateDoesNotRecreateExpiredUnresolvedParent(t *testing.T) {
	client := &fakeRecurringCreateClient{}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example"}
	key := time.Now().UTC().Add(-8*24*time.Hour).Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	_, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.ErrorIs(t, err, ErrRecurringRetryExpired)
	assert.Zero(t, client.creates)
}

func TestRecurringCreateRequiresCronSupportBeforeParentWrite(t *testing.T) {
	client := &fakeRecurringCreateClient{catalogErr: taskclient.ErrIncompatible}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example"}
	key := time.Now().UTC().Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	_, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.ErrorIs(t, err, taskclient.ErrIncompatible)
	assert.Zero(t, client.creates)
}

func TestRecurringCreateRejectsChangedProjectBeforeWriting(t *testing.T) {
	client := &fakeRecurringCreateClient{}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "new-project"}
	key := time.Now().UTC().Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "old-project", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	_, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.ErrorIs(t, err, ErrRecurringProjectChanged)
	assert.Zero(t, client.creates)
}

func TestRecurringCreateRejectsInactiveReplayedJob(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "paused", true: "deleted"}[deleted], func(t *testing.T) {
			client := &fakeRecurringCreateClient{loseJob: true}
			service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example"}
			key := time.Now().UTC().Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
			input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
			_, err := service.CreateRecurring(t.Context(), 1, key, input)
			require.ErrorIs(t, err, ErrScheduleOutcomeUnknown)
			client.job.Deleted, client.job.Enabled = deleted, false
			_, err = service.CreateRecurring(t.Context(), 1, key, input)
			assert.ErrorIs(t, err, taskclient.ErrConflict)
		})
	}
}

func TestRecurringCreateRecoversConfirmedJobAfterLostResponse(t *testing.T) {
	client := &fakeRecurringCreateClient{loseJob: true}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example"}
	key := time.Now().UTC().Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	partial, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.ErrorIs(t, err, ErrScheduleOutcomeUnknown)
	assert.Equal(t, fixtureParentUID, partial.Parent.UID)
	result, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.NoError(t, err)
	require.NotNil(t, result.Schedule)
	assert.Equal(t, client.job.UID, result.Schedule.UID)
	assert.Equal(t, 2, client.creates, "one parent and one job are created")
}

func TestRecurringCreateReplaysMarkerAfterIdempotencyWindow(t *testing.T) {
	client := &fakeRecurringCreateClient{}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example", Now: func() time.Time { return now }}
	key := now.Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	first, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.NoError(t, err)
	now = now.Add(8 * 24 * time.Hour)
	second, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.NoError(t, err)
	require.NotNil(t, first.Schedule)
	require.NotNil(t, second.Schedule)
	assert.Equal(t, first.Parent.UID, second.Parent.UID)
	assert.Equal(t, first.Schedule.UID, second.Schedule.UID)
	assert.Equal(t, 2, client.creates)
}

func TestRecurringCreateRejectsChangedRequestForSameKey(t *testing.T) {
	client := &fakeRecurringCreateClient{}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example"}
	key := time.Now().UTC().Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	_, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.NoError(t, err)
	input.Settings.Recipient = "different"
	_, err = service.CreateRecurring(t.Context(), 1, key, input)
	require.ErrorIs(t, err, taskclient.ErrConflict)
	assert.Equal(t, 2, client.creates)
}

func TestRecurringCreateRecoversAfterPersonMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := &fakeRecurringCreateClient{loseJob: true}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"original"}}, Project: "example"}
	key := time.Now().UTC().Format(time.RFC3339Nano) + "/550e8400-e29b-41d4-a716-446655440000"
	input := RecurringCreateInput{Project: "example", Title: "Review", Owner: "synthetic", Settings: ScheduleInput{TriggerKind: "interval", IntervalSeconds: 60, Timezone: "UTC", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "latest"}}
	_, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.ErrorIs(err, ErrScheduleOutcomeUnknown)
	service.People = fakeIdentityStore{uids: []string{"survivor", "original"}}
	result, err := service.CreateRecurring(t.Context(), 1, key, input)
	require.NoError(err)
	require.NotNil(result.Schedule)
	assert.Equal(client.job.UID, result.Schedule.UID)
	assert.Equal(2, client.creates)
}
