package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"strings"
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

func (f *agendaArchiveFixture) ListCommitmentAgendaCandidatesContext(context.Context, string, time.Time, int) ([]store.PersonCommitment, error) {
	return nil, nil
}

func (f *agendaArchiveFixture) GetCommitmentAgendaContext(context.Context, int64) (store.CommitmentAgendaSuggestion, error) {
	if f.item.CommitmentID == 0 {
		return f.item, sql.ErrNoRows
	}
	return f.item, nil
}

func (f *agendaArchiveFixture) ListPendingCommitmentAgendaContext(context.Context, string, time.Time, int) ([]store.CommitmentAgendaSuggestion, error) {
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

func (f *agendaArchiveFixture) MarkCommitmentAgendaAttemptContext(context.Context, store.CommitmentAgendaSuggestion, time.Time, bool) (bool, error) {
	if f.closed {
		return false, nil
	}
	f.item.Attempted = true
	return true, nil
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
	lookup func(string, kataissues.CreateInput)
}

func (f *agendaIssuesFixture) Create(_ context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error) {
	return f.create(key, input)
}

func (f *agendaIssuesFixture) FindCreated(_ context.Context, key string, input kataissues.CreateInput) (kataissues.Result, bool, error) {
	if f.lookup != nil {
		f.lookup(key, input)
	}
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
		archive: archive, evidence: kataevidence.New(archive), enabled: true, destination: "fixture-destination", origin: "https://archive.example.test",
		issues: func(context.Context) (commitmentAgendaIssues, error) { return issues, nil },
	}
	return agenda, archive, issues
}

func TestCommitmentAgendaSettlesSavedIntent(t *testing.T) {
	t.Run("passes exact citation and links after create", func(t *testing.T) {
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
			a.Equal("[Open the original message](https://archive.example.test/messages/19)", input.Brief)
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
	})
	for _, attempted := range []bool{false, true} {
		name := "closed before submission"
		if attempted {
			name = "created before the reply was lost"
		}
		t.Run(name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			agenda, archive, issues := newCommitmentAgendaFixture()
			if attempted {
				issues.lookup = func(key string, input kataissues.CreateInput) {
					a.Equal(archive.item.IdempotencyKey, key)
					a.Equal(archive.item.Request, input)
				}
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
				a.Equal(issues.found.Issue.UID, archive.item.IssueUID)
				a.Equal(issues.found.Issue.QualifiedRef, archive.item.IssueRef)
			} else {
				a.Equal(store.CommitmentAgendaRetired, archive.item.State)
			}
		})
	}
}

func TestCommitmentAgendaPreservesLongUnicodeCitation(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	agenda, archive, _ := newCommitmentAgendaFixture()
	quote := strings.Repeat("é", kataevidence.MaxChars+5)
	archive.commitment.Quote = quote
	archive.commitment.End = archive.commitment.Start + kataevidence.MaxChars + 5
	archive.source.Text = "Très bien. " + quote + " Thanks!"
	sum := sha256.Sum256([]byte(archive.source.Text))
	hash := hex.EncodeToString(sum[:])
	archive.source.Reference.Message.BodySHA256 = hash
	item, err := agenda.enqueue(t.Context(), archive.commitment)
	r.NoError(err)
	a.Equal(quote, item.Quote)
	r.Len(item.Request.Evidence, 2)
	first, second := item.Request.Evidence[0].Message, item.Request.Evidence[1].Message
	a.Equal(int(archive.commitment.Start), first.StartRune)
	a.Equal(first.EndRune, second.StartRune)
	a.Equal(int(archive.commitment.End), second.EndRune)
	a.Equal(hash, first.BodySHA256)
	a.Equal(hash, second.BodySHA256)
}
