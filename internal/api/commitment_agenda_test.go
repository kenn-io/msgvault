package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

type agendaArchiveFixture struct {
	commitmentAgendaArchive

	commitment store.PersonCommitment
	item       store.CommitmentAgendaSuggestion
	source     kataevidence.SourceRecord
	closed     bool
}

func (f *agendaArchiveFixture) GetOpenPersonCommitmentContext(context.Context, int64, time.Time) (store.PersonCommitment, error) {
	if f.closed {
		return store.PersonCommitment{}, sql.ErrNoRows
	}
	return f.commitment, nil
}

func (f *agendaArchiveFixture) ListCommitmentAgendaCandidatesContext(context.Context, time.Time, int) ([]store.PersonCommitment, error) {
	return nil, nil
}

func (f *agendaArchiveFixture) GetCommitmentAgendaContext(context.Context, int64) (store.CommitmentAgendaSuggestion, error) {
	if f.item.CommitmentID == 0 {
		return f.item, sql.ErrNoRows
	}
	return f.item, nil
}

func (f *agendaArchiveFixture) ListPendingCommitmentAgendaContext(context.Context, int) ([]store.CommitmentAgendaSuggestion, error) {
	if f.item.State == store.CommitmentAgendaPending {
		return []store.CommitmentAgendaSuggestion{f.item}, nil
	}
	return nil, nil
}

func (f *agendaArchiveFixture) EnqueueCommitmentAgendaContext(_ context.Context, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error) {
	item.State = store.CommitmentAgendaPending
	f.item = item
	return item, nil
}

func (f *agendaArchiveFixture) MarkCommitmentAgendaAttemptContext(context.Context, int64, time.Time) (bool, error) {
	f.item.Attempted = true
	return !f.closed, nil
}

func (f *agendaArchiveFixture) RetireCommitmentAgendaContext(context.Context, int64) error {
	f.item.State = store.CommitmentAgendaRetired
	return nil
}

func (f *agendaArchiveFixture) LinkCommitmentAgendaContext(_ context.Context, _ int64, uid, ref string) (store.CommitmentAgendaSuggestion, error) {
	f.item.State, f.item.IssueUID, f.item.IssueRef = store.CommitmentAgendaLinked, uid, ref
	return f.item, nil
}

func (f *agendaArchiveFixture) ListPersonUIDsContext(context.Context, int64) ([]string, error) {
	return []string{"person-vcard-uid"}, nil
}

func (f *agendaArchiveFixture) LoadKataEvidenceSource(context.Context, kataevidence.Selector) (kataevidence.SourceRecord, error) {
	return f.source, nil
}

type agendaIssuesFixture struct {
	create func(string, kataissues.CreateInput) (kataissues.Result, error)
	found  kataissues.Result
}

func (f *agendaIssuesFixture) Create(_ context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error) {
	return f.create(key, input)
}

func (f *agendaIssuesFixture) FindCreated(context.Context, string, kataissues.CreateInput) (kataissues.Result, bool, error) {
	return f.found, f.found.Issue.UID != "", nil
}

func newCommitmentAgendaFixture() (*CommitmentAgenda, *agendaArchiveFixture, *agendaIssuesFixture) {
	const prefix, quote = "Très bien. ", "I will send the draft."
	body := prefix + quote + " Thanks!"
	sum := sha256.Sum256([]byte(body))
	archive := &agendaArchiveFixture{
		commitment: store.PersonCommitment{
			ID: 12, PersonID: 7, TargetKey: "commitment:target", AnchorMessageID: 19,
			Quote: quote, Start: 11, End: 33, Summary: "Send the draft",
		},
		source: kataevidence.SourceRecord{
			Text: body,
			Reference: kataevidence.Reference{
				Version: 1, Kind: "message", ArchiveUID: "archive", MessageID: 19,
				SourceType: "email", SourceIdentifier: "owner@example.test", SourceMessageID: "message-19",
				Message: &kataevidence.MessageReference{BodySHA256: hex.EncodeToString(sum[:])},
			},
		},
	}
	issues := &agendaIssuesFixture{}
	agenda := &CommitmentAgenda{
		archive: archive, evidence: kataevidence.New(archive), enabled: true, destination: "fixture-destination",
		issues: func(context.Context) (commitmentAgendaIssues, error) { return issues, nil },
	}
	return agenda, archive, issues
}

func TestCommitmentAgendaFreezesExactCitationAndLinksThroughClosure(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	agenda, archive, issues := newCommitmentAgendaFixture()
	task := taskclient.KataTask{UID: "issue-1", QualifiedRef: "agenda#12", Metadata: map[string]any{
		personagenda.PersonMetadataKey: "person-vcard-uid", personagenda.ListMetadataKey: "commitments",
	}}
	calls := 0
	issues.create = func(key string, input kataissues.CreateInput) (kataissues.Result, error) {
		calls++
		a.Equal("commitment-agenda:c12", key)
		a.Equal("person-vcard-uid", input.PersonUID)
		r.Len(input.Evidence, 1)
		a.Equal(11, input.Evidence[0].Message.StartRune)
		a.Equal(33, input.Evidence[0].Message.EndRune)
		a.Equal("I will send the draft.", archive.item.Quote)
		archive.closed = true
		return kataissues.Result{Issue: task}, nil
	}
	now := time.Now()
	item, err := agenda.Manual(t.Context(), 12, now)
	r.NoError(err)
	a.Equal(task.UID, item.IssueUID)
	item, err = agenda.Manual(t.Context(), 12, now)
	r.NoError(err)
	a.Equal(task.QualifiedRef, item.IssueRef)
	a.Equal(1, calls)
}

func TestCommitmentAgendaDrainRetiresClosedAndRecoversCompletedCreate(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		name := "closed before submission"
		if attempted {
			name = "created before the reply was lost"
		}
		t.Run(name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			agenda, archive, issues := newCommitmentAgendaFixture()
			if attempted {
				issues.create = func(string, kataissues.CreateInput) (kataissues.Result, error) {
					issues.found.Issue = taskclient.KataTask{
						UID: "issue-1", QualifiedRef: "agenda#12",
						Metadata: map[string]any{personagenda.PersonMetadataKey: "person-vcard-uid", personagenda.ListMetadataKey: "commitments"},
					}
					return kataissues.Result{}, io.ErrUnexpectedEOF
				}
				_, err := agenda.Manual(t.Context(), 12, time.Now())
				r.ErrorIs(err, io.ErrUnexpectedEOF)
			} else {
				_, err := agenda.enqueue(t.Context(), archive.commitment)
				r.NoError(err)
			}
			archive.closed = true
			issues.create = func(string, kataissues.CreateInput) (kataissues.Result, error) {
				r.FailNow("closed commitments cannot create new Kata issues")
				return kataissues.Result{}, nil
			}
			r.NoError(agenda.AutoRun(t.Context(), time.Now()))
			if attempted {
				a.Equal(store.CommitmentAgendaLinked, archive.item.State)
			} else {
				a.Equal(store.CommitmentAgendaRetired, archive.item.State)
			}
		})
	}
}
