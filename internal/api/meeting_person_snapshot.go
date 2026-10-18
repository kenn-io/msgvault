package api

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

type meetingPersonSnapshotBackend interface {
	BeginReadSnapshotContext(ctx context.Context) (context.Context, func(), error)
	GetMeetingPersonContext(ctx context.Context, id int64) (*store.Person, error)
	ListMeetingPersonContactPointsContext(ctx context.Context, id int64) ([]store.PersonContactPoint, error)
	MeetingClusterMembersContext(ctx context.Context, id int64) ([]int64, error)
	MeetingPersonForParticipantsContext(ctx context.Context, ids []int64) (*store.Person, error)
	MeetingParticipantEmailsContext(ctx context.Context, ids []int64) ([]string, error)
}

type meetingSnapshotPersonResolver struct {
	backend meetingPersonSnapshotBackend
	ctx     context.Context
}

func (a meetingSnapshotPersonResolver) GetPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return a.backend.GetMeetingPersonContext(ctx, id)
}
func (a meetingSnapshotPersonResolver) ClusterMembers(id int64) ([]int64, error) {
	return a.backend.MeetingClusterMembersContext(a.ctx, id)
}
func (a meetingSnapshotPersonResolver) PersonForParticipantsContext(ctx context.Context, ids []int64) (*store.Person, error) {
	return a.backend.MeetingPersonForParticipantsContext(ctx, ids)
}
