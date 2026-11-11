package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
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
	sourceErr  error
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

func (f *agendaArchiveFixture) ListPendingCommitmentAgendaContext(context.Context, time.Time, int) ([]store.CommitmentAgendaSuggestion, error) {
	if f.item.State == store.CommitmentAgendaPending {
		return []store.CommitmentAgendaSuggestion{f.item}, nil
	}
	return nil, nil
}

func (f *agendaArchiveFixture) EnqueueCommitmentAgendaContext(_ context.Context, item store.CommitmentAgendaSuggestion) (store.CommitmentAgendaSuggestion, error) {
	item.State = store.CommitmentAgendaPending
	item.IdempotencyKey = "commitment-agenda:fixture"
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

func (f *agendaArchiveFixture) RetireCommitmentAgendaContext(context.Context, string) error {
	f.item.State = store.CommitmentAgendaRetired
	return nil
}

func (f *agendaArchiveFixture) LinkCommitmentAgendaContext(_ context.Context, _ string, uid, ref string) (store.CommitmentAgendaSuggestion, error) {
	if f.item.CommitmentID == 0 {
		return store.CommitmentAgendaSuggestion{}, sql.ErrNoRows
	}
	f.item.State, f.item.IssueUID, f.item.IssueRef = store.CommitmentAgendaLinked, uid, ref
	return f.item, nil
}

func (f *agendaArchiveFixture) ListPersonUIDsContext(context.Context, int64) ([]string, error) {
	return []string{"person-vcard-uid"}, nil
}

func (f *agendaArchiveFixture) LoadKataEvidenceSource(context.Context, kataevidence.Selector) (kataevidence.SourceRecord, error) {
	return f.source, f.sourceErr
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
		archive: archive, evidence: kataevidence.New(archive), enabled: true, origin: "https://archive.example.test",
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
			a.Equal(archive.item.IdempotencyKey, key)
			a.Equal("person-vcard-uid", input.PersonUID)
			r.Len(input.Evidence, 1)
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
	t.Run("receipt removed while remote create succeeds", func(t *testing.T) {
		r, a := require.New(t), assert.New(t)
		agenda, archive, issues := newCommitmentAgendaFixture()
		task := taskclient.KataTask{UID: "created-then-receipt-removed", QualifiedRef: "agenda#19"}
		issues.create = func(string, kataissues.CreateInput) (kataissues.Result, error) {
			// A losing receipt can disappear during a merge while this create
			// is in flight. The accepted single-receipt policy keeps the winner.
			archive.item = store.CommitmentAgendaSuggestion{}
			return kataissues.Result{Issue: task}, nil
		}
		item, err := agenda.Manual(t.Context(), 12, time.Now())
		r.NoError(err, "a successfully created remote issue must be returned even when its local receipt was removed")
		a.Equal(task.UID, item.IssueUID)
		a.Equal(task.QualifiedRef, item.IssueRef)
		a.Zero(archive.item.CommitmentID, "the response must not recreate the losing receipt")
	})
	t.Run("closed before submission", func(t *testing.T) {
		r, a := require.New(t), assert.New(t)
		agenda, archive, issues := newCommitmentAgendaFixture()
		_, err := agenda.enqueue(t.Context(), archive.commitment)
		r.NoError(err)
		archive.closed = true
		issues.create = func(string, kataissues.CreateInput) (kataissues.Result, error) {
			r.FailNow("closed commitments cannot create new Kata issues")
			return kataissues.Result{}, nil
		}
		r.NoError(agenda.AutoRun(t.Context(), time.Now()))
		a.Equal(store.CommitmentAgendaRetired, archive.item.State)
	})
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

func TestCommitmentAgendaRefreshesOnlyUnchangedPassages(t *testing.T) {
	for _, change := range []string{"outside quote", "quote", "occurrence", "archive read failure"} {
		t.Run(change, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			agenda, archive, issues := newCommitmentAgendaFixture()
			frozen, err := agenda.enqueue(t.Context(), archive.commitment)
			r.NoError(err)
			archive.source.Text += " New ending."
			if change == "quote" {
				archive.source.Text = strings.Replace(archive.source.Text, "draft", "notes", 1)
			}
			if change == "occurrence" {
				archive.source.Reference.SourceMessageID = "another-message"
			}
			if change == "archive read failure" {
				archive.sourceErr = io.ErrUnexpectedEOF
			}
			sum := sha256.Sum256([]byte(archive.source.Text))
			archive.source.Reference.Message.BodySHA256 = hex.EncodeToString(sum[:])
			calls := 0
			issues.create = func(key string, input kataissues.CreateInput) (kataissues.Result, error) {
				calls++
				a.Equal(frozen.IdempotencyKey, key)
				if calls == 1 {
					a.Equal(frozen.Request, input)
					return kataissues.Result{}, kataevidence.ErrChanged
				}
				a.Equal(frozen.Request.Title, input.Title)
				a.Equal(frozen.Request.Brief, input.Brief)
				a.Equal(frozen.Request.PersonUID, input.PersonUID)
				a.Equal(frozen.Request.List, input.List)
				r.Len(input.Evidence, 1)
				a.Equal(kataevidence.PassageLocation(frozen.Request.Evidence[0]), kataevidence.PassageLocation(input.Evidence[0]))
				a.Equal(archive.source.Reference.Message.BodySHA256, input.Evidence[0].Message.BodySHA256)
				return kataissues.Result{Issue: taskclient.KataTask{UID: "issue-1", QualifiedRef: "agenda#12"}}, nil
			}
			item, err := agenda.Manual(t.Context(), archive.commitment.ID, time.Now())
			if change == "outside quote" {
				r.NoError(err)
				a.Equal("issue-1", item.IssueUID)
				a.Equal(2, calls)
				a.Equal(frozen.Request, item.Request, "the durable request remains frozen")
			} else {
				wantError := map[string]error{
					"quote": errCommitmentAgendaCitation, "occurrence": errCommitmentAgendaCitation,
					"archive read failure": kataevidence.ErrArchiveUnavailable,
				}[change]
				r.ErrorIs(err, wantError)
				a.Equal(1, calls)
				if change == "archive read failure" {
					r.ErrorIs(err, io.ErrUnexpectedEOF)
					a.Equal(store.CommitmentAgendaPending, archive.item.State)
				}
			}
		})
	}
}

func TestCommitmentAgendaChangedCitationRecoversOrConflicts(t *testing.T) {
	for _, trigger := range []string{"closed", "source changed", "target", "body", "shortened body"} {
		for _, found := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/found=%t", trigger, found), func(t *testing.T) {
				r, a := require.New(t), assert.New(t)
				agenda, archive, issues := newCommitmentAgendaFixture()
				calls := 0
				issues.lookup = func(key string, input kataissues.CreateInput) {
					a.Equal(archive.item.IdempotencyKey, key)
					a.Equal(archive.item.Request, input)
				}
				issues.create = func(string, kataissues.CreateInput) (kataissues.Result, error) {
					calls++
					return kataissues.Result{}, io.ErrUnexpectedEOF
				}
				_, err := agenda.Manual(t.Context(), 12, time.Now())
				r.ErrorIs(err, io.ErrUnexpectedEOF)
				if found {
					issues.found.Issue = taskclient.KataTask{UID: "same-issue", QualifiedRef: "agenda#12", Metadata: map[string]any{personagenda.PersonMetadataKey: "person-vcard-uid", personagenda.ListMetadataKey: "commitments"}}
				}
				switch trigger {
				case "closed":
					archive.closed = true
					// Recovery must link even after the owner moved or unlinked the issue.
					if found {
						issues.found.Issue.Metadata = map[string]any{personagenda.ListMetadataKey: "later"}
					}
				case "source changed":
					// The real Store hides a commitment whose citation no longer
					// verifies, just as it hides an owner-closed commitment.
					archive.closed = true
					archive.source.Text = strings.Replace(archive.source.Text, "draft", "notes", 1)
				case "target":
					archive.commitment.TargetKey = "survivor-message-target"
				case "body", "shortened body":
					archive.source.Text = strings.Replace(archive.source.Text, "draft", "notes", 1)
					if trigger == "shortened body" {
						archive.source.Text = "Gone."
					}
					issues.create = func(string, kataissues.CreateInput) (kataissues.Result, error) {
						calls++
						return kataissues.Result{}, kataevidence.ErrChanged
					}
				}
				var item store.CommitmentAgendaSuggestion
				if trigger == "closed" {
					err = agenda.AutoRun(t.Context(), time.Now())
					item = archive.item
				} else {
					item, err = agenda.Manual(t.Context(), 12, time.Now())
				}
				if found {
					r.NoError(err)
					a.Equal(issues.found.Issue.UID, item.IssueUID)
					a.Equal(issues.found.Issue.QualifiedRef, item.IssueRef)
				} else {
					if trigger == "closed" {
						r.NoError(err)
					} else {
						r.ErrorIs(err, errCommitmentAgendaCitation)
					}
					a.Equal(store.CommitmentAgendaRetired, archive.item.State)
				}
				expectedCalls := 1
				if trigger == "body" || trigger == "shortened body" {
					expectedCalls = 2
				}
				a.Equal(expectedCalls, calls, "citation changes cannot authorize another create with refreshed evidence")
			})
		}
	}
}
