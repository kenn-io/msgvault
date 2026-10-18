package loops

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/textutil"
)

type ServiceOption func(*Service)

func WithKata(kata KataIssues) ServiceOption { return func(s *Service) { s.kata = kata } }

// PrepareLoopEvidence is shared by explicit tracking and automatic outboxes.
// Quote offsets belong to extractor excerpts, so only quote text can select
// the corresponding archived body passage safely.
func PrepareLoopEvidence(ctx context.Context, kata KataIssues, row store.OpenLoop, policy EvidencePolicy) ([]kataevidence.Reference, string, error) {
	if row.AnchorMessageID == nil {
		return nil, "", kataevidence.ErrUnavailable
	}
	if kata == nil {
		return nil, "", ErrKataUnavailable
	}
	var evidence []Evidence
	if err := json.Unmarshal(row.EvidenceJSON, &evidence); err != nil {
		return nil, "", fmt.Errorf("decode loop evidence: %w", err)
	}
	window := func(id int64) kataevidence.Selector {
		start, maxChars := 0, 1000
		return kataevidence.Selector{Kind: "message", MessageID: id, StartRune: &start, MaxChars: &maxChars}
	}
	selector := window(*row.AnchorMessageID)
	if len(evidence) > 0 && evidence[0].Quote != "" {
		selector = kataevidence.Selector{Kind: "message", MessageID: *row.AnchorMessageID, Quote: evidence[0].Quote}
	}
	prepared, err := kata.Prepare(ctx, []kataevidence.Selector{selector})
	note := ""
	if policy.FallbackToWindow && selector.Quote != "" && (errors.Is(err, kataevidence.ErrQuoteAmbiguous) || errors.Is(err, kataevidence.ErrQuoteNotFound)) {
		prepared, err = kata.Prepare(ctx, []kataevidence.Selector{window(*row.AnchorMessageID)})
		note = "quote could not be cited exactly; cited the start of the message"
	}
	if err != nil {
		return nil, "", err
	}
	refs := make([]kataevidence.Reference, 0, len(prepared)+1)
	for _, item := range prepared {
		refs = append(refs, item.Reference)
	}
	if row.LastActivityMessageID != nil && *row.LastActivityMessageID != *row.AnchorMessageID {
		latest, err := kata.Prepare(ctx, []kataevidence.Selector{window(*row.LastActivityMessageID)})
		if err != nil {
			return nil, "", err
		}
		for _, item := range latest {
			refs = append(refs, item.Reference)
		}
	}
	return refs, note, nil
}

func (s *Service) Track(ctx context.Context, id int64, req TrackRequest) (TrackResponse, error) {
	if err := s.check(); err != nil {
		return TrackResponse{}, err
	}
	req.IssueRef = strings.TrimSpace(req.IssueRef)
	if req.IssueRef != "" && (req.Title != "" || req.List != "") || !utf8.ValidString(req.Title) || utf8.RuneCountInString(req.Title) > 512 {
		return TrackResponse{}, kataissues.ErrInvalidRequest
	}
	row, err := s.st.GetOpenLoopContext(ctx, id)
	if err != nil {
		return TrackResponse{}, err
	}
	if row.Status == "closed" {
		return TrackResponse{}, store.ErrLoopAlreadyClosed
	}
	if row.Kata != nil && req.IssueRef == "" {
		return s.trackResult(ctx, row, cachedIssue(*row.Kata), true, "", false)
	}
	if s.kata == nil {
		return TrackResponse{}, ErrKataUnavailable
	}
	if row.Kata != nil {
		issue, err := s.kata.Get(ctx, req.IssueRef)
		if err != nil {
			return TrackResponse{}, err
		}
		if issue.UID != row.Kata.IssueUID {
			return TrackResponse{}, &AlreadyTrackedError{Kata: publicLoopKata(*row.Kata)}
		}
		return s.trackResult(ctx, row, issueReceipt(issue), true, "", false)
	}
	if req.IssueRef == "" {
		if req.List != "" && row.CounterpartyPersonID == nil {
			return TrackResponse{}, kataissues.ErrInvalidRequest
		}
		if _, err := personagenda.NormalizeList(req.List); err != nil {
			return TrackResponse{}, kataissues.ErrInvalidRequest
		}
	}
	refs, note, err := PrepareLoopEvidence(ctx, s.kata, row, EvidencePolicy{FallbackToWindow: true})
	if err != nil {
		return TrackResponse{}, err
	}
	var issue taskclient.KataTask
	replayed := false
	via := "existing"
	if req.IssueRef != "" {
		issue, err = s.kata.Link(ctx, req.IssueRef, refs)
	} else {
		title := strings.TrimSpace(req.Title)
		if title == "" {
			loop, e := publicLoop(row, s.now().UTC())
			if e != nil {
				return TrackResponse{}, e
			}
			title = DefaultTrackTitle(loop)
		}
		input := kataissues.CreateInput{Title: title, Brief: fmt.Sprintf("Open loop %d (%s, %s) in msgvault, owed since %s; conversation %d.", row.ID, row.Kind, row.Channel, row.OwedSinceAt.Format("2006-01-02"), row.ConversationID), List: req.List, PersonID: row.CounterpartyPersonID, Evidence: refs}
		var result kataissues.Result
		result, err = s.kata.Create(ctx, fmt.Sprintf("loop:%d", id), input)
		if errors.Is(err, kataissues.ErrIdempotencyConflict) && result.Issue.UID != "" {
			note = joinNotes("request differed from the issue this loop filed; linked to it", note)
			err = nil
			replayed = true
		}
		issue = result.Issue
		replayed = replayed || result.Replayed
		via = "created"
	}
	if err != nil {
		return TrackResponse{}, err
	}
	if issue.UID == "" {
		return TrackResponse{}, taskclient.ErrInvalidResponse
	}
	now := s.now().UTC()
	link := store.LoopKataLink{LoopID: id, IssueUID: issue.UID, IssueRef: issue.QualifiedRef, IssueTitle: safeIssueTitle(issue.Title), IssueStatus: issue.Status, IssueWebURL: personagenda.SafeWebURL(issue.WebURL), LinkedVia: via, LinkedAt: now, CheckedAt: &now, NextAttemptAt: &now}
	personUID, _ := issue.Metadata[personagenda.PersonMetadataKey].(string)
	current, inserted, err := s.st.InsertLoopKataLinkContext(ctx, link, personUID)
	if err != nil {
		return TrackResponse{}, err
	}
	if current.IssueUID != issue.UID {
		return TrackResponse{}, &AlreadyTrackedError{Kata: publicLoopKata(current)}
	}
	row, err = s.st.GetOpenLoopContext(ctx, id)
	if err != nil {
		return TrackResponse{}, err
	}
	if req.IssueRef != "" && row.CounterpartyPersonID != nil && personUID == "" {
		note = joinNotes(note, "the issue has no person agenda link; use person agenda link to add it")
	}
	return s.trackResult(ctx, row, issueReceipt(issue), replayed || !inserted, note, true)
}

func (s *Service) trackResult(ctx context.Context, row store.OpenLoop, issue KataIssue, replayed bool, note string, live bool) (TrackResponse, error) {
	detail, err := s.actionResult(ctx, row, nil)
	if detail.Loop.Kata != nil {
		detail.Loop.Kata.Live = live
	}
	return TrackResponse{DetailResponse: detail, Issue: issue, Replayed: replayed, Note: note}, err
}

func (s *Service) Untrack(ctx context.Context, id int64) error {
	if err := s.check(); err != nil {
		return err
	}
	err := s.st.DeleteLoopKataLinkContext(ctx, id)
	if errors.Is(err, store.ErrLoopKataLinkNotFound) {
		return store.ErrLoopNotFound
	}
	return err
}

func publicLoopKata(link store.LoopKataLink) LoopKata {
	return LoopKata{IssueUID: link.IssueUID, QualifiedRef: link.IssueRef, Title: safeIssueTitle(link.IssueTitle), Status: link.IssueStatus, WebURL: personagenda.SafeWebURL(link.IssueWebURL), LinkedVia: link.LinkedVia, LinkedAt: link.LinkedAt, CheckedAt: link.CheckedAt, Attempts: link.Attempts, NextAttemptAt: link.NextAttemptAt, LastError: link.LastError, ClosedNotifiedAt: link.ClosedNotifiedAt, ClosedLoopAt: link.BridgeClosedLoopAt}
}
func issueReceipt(issue taskclient.KataTask) KataIssue {
	return KataIssue{UID: issue.UID, Ref: issue.Ref, QualifiedRef: issue.QualifiedRef, Project: issue.Project, Title: safeIssueTitle(issue.Title), Status: issue.Status, Revision: issue.Revision, WebURL: personagenda.SafeWebURL(issue.WebURL)}
}
func cachedIssue(link store.LoopKataLink) KataIssue {
	project, ref, _ := strings.Cut(link.IssueRef, "#")
	return KataIssue{UID: link.IssueUID, Ref: ref, QualifiedRef: link.IssueRef, Project: project, Title: safeIssueTitle(link.IssueTitle), Status: link.IssueStatus, WebURL: personagenda.SafeWebURL(link.IssueWebURL)}
}
func safeIssueTitle(title string) string {
	return textutil.TruncateRunes(strings.ToValidUTF8(title, ""), 512)
}
func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

// refreshKata updates only the observed installation. A concurrent untrack or
// relink is reread for the response and can never receive this stale cache.
func (s *Service) refreshKata(ctx context.Context, row store.OpenLoop) (store.OpenLoop, bool) {
	if s.kata == nil || row.Kata == nil {
		return row, false
	}
	link := *row.Kata
	issue, err := s.kata.Get(ctx, link.IssueUID)
	live := err == nil
	link.UpdatedAt = s.now().UTC()
	if live {
		link.IssueRef = issue.QualifiedRef
		link.IssueTitle = safeIssueTitle(issue.Title)
		link.IssueStatus = issue.Status
		link.IssueWebURL = personagenda.SafeWebURL(issue.WebURL)
		now := s.now().UTC()
		link.CheckedAt = &now
		link.UpdatedAt = now
		link.LastError = ""
	} else {
		link.LastError = kataFailure(err).Error()
	}
	var writeErr error
	if live {
		writeErr = s.st.RefreshLoopKataLinkContext(ctx, link)
	} else {
		writeErr = s.st.RecordLoopKataRefreshErrorContext(ctx, link)
	}
	if writeErr != nil {
		return row, false
	}
	current, err := s.st.GetOpenLoopContext(ctx, row.ID)
	if err != nil {
		return row, false
	}
	return current, live && current.Kata != nil && current.Kata.LinkToken == link.LinkToken
}

func kataFailure(err error) error {
	switch {
	case errors.Is(err, taskclient.ErrNotFound):
		return errors.New("issue in Kata is unavailable")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("request to Kata timed out")
	case errors.Is(err, context.Canceled):
		return errors.New("request to Kata canceled")
	case errors.Is(err, taskclient.ErrAuthenticationRequired):
		return errors.New("authentication with Kata failed")
	default:
		return errors.New("request to Kata failed")
	}
}
