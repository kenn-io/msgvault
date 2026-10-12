package kataissues

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/taskclient"
)

// ContextPageSize is the maximum number of passages one read returns.
const ContextPageSize = 10

// Context is one page of the passages an issue cites, each with its state in
// the archive today.
type Context struct {
	Issue    taskclient.KataTask
	Passages []ContextPassage
	// NextOffset is where the next page starts; zero means this is the last.
	NextOffset int
}

// ContextPassage keeps the issue's saved words separate from current archive text.
type ContextPassage struct {
	kataevidence.Resolution

	SavedQuote string `json:"saved_quote,omitzero" doc:"Quoted words recovered from the issue body or comments when archive evidence is not available"`
}

// Context reads the passages an issue cites, in the order they were added,
// starting at offset; an offset past the last passage reads an empty page.
// It returns up to ContextPageSize passages, admitting transcript passages while
// their accumulated read time is below kataevidence.DocbankTimeout.
// It never writes to Kata or starts archive processing.
func (s *Service) Context(ctx context.Context, issueRef string, offset int) (Context, error) {
	project, issueRef, err := s.splitRef(issueRef)
	if err != nil {
		return Context{}, err
	}
	issue, envelope, err := s.issueEvidence(ctx, project, issueRef)
	if err != nil {
		return Context{}, err
	}
	// Each passage keeps its entries in the order they were added.
	var passages [][]Entry
	at := map[string]int{}
	for _, entry := range envelope.Entries {
		i, ok := at[entry.Passage]
		if !ok {
			i, at[entry.Passage] = len(passages), len(passages)
			passages = append(passages, nil)
		}
		passages[i] = append(passages[i], entry)
	}
	result := Context{Issue: issue}
	quotes := savedQuotes(issue.Body)
	for _, body := range issue.CommentBodies {
		quotes = append(quotes, savedQuotes(body)...)
	}
	start := min(offset, len(passages))
	end := min(start+ContextPageSize, len(passages))
	var docbankElapsed time.Duration
	for i, entries := range passages[start:end] {
		if err := ctx.Err(); err != nil {
			return Context{}, err
		}
		remote := entries[len(entries)-1].Reference.Kind == "docbank_rendition"
		if remote && docbankElapsed >= kataevidence.DocbankTimeout {
			end = start + i
			break
		}
		started := time.Now()
		resolution, err := s.resolvePassage(ctx, entries)
		if remote {
			docbankElapsed += time.Since(started)
		}
		if err != nil {
			return Context{}, err
		}
		passage := ContextPassage{Resolution: resolution}
		if resolution.State != kataevidence.Available {
			for _, entry := range slices.Backward(entries) {
				if entry.Pending != nil {
					passage.SavedQuote = entry.Pending.Quote
					break
				}
			}
			if passage.SavedQuote == "" {
				entry := entries[len(entries)-1]
				for _, quote := range quotes {
					if kataevidence.PassageID(entry.Reference, quote) == entry.Passage {
						passage.SavedQuote = quote
						break
					}
				}
			}
		}
		result.Passages = append(result.Passages, passage)
	}
	if end < len(passages) {
		result.NextOffset = end
	}
	return result, nil
}

// savedQuotes reverses renderQuote without relying on its repeated source labels.
func savedQuotes(body string) []string {
	lines := strings.Split(body, "\n")
	var quotes []string
	for i := 0; i+2 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "Evidence: ") || lines[i+1] != "" || !strings.HasPrefix(lines[i+2], "> ") {
			continue
		}
		i += 2
		var words []string
		for i < len(lines) && strings.HasPrefix(lines[i], "> ") {
			words = append(words, strings.TrimPrefix(lines[i], "> "))
			i++
		}
		quotes = append(quotes, strings.Join(words, "\n"))
		i--
	}
	return quotes
}

// resolvePassage reads a passage's entries newest first and keeps the first
// available one, so a passage re-linked after a re-sync reads as available
// rather than also as changed. Otherwise it reports the newest entry's state.
func (s *Service) resolvePassage(ctx context.Context, entries []Entry) (kataevidence.Resolution, error) {
	parent := ctx
	if entries[len(entries)-1].Reference.Kind == "docbank_rendition" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, kataevidence.DocbankTimeout)
		defer cancel()
	}
	var newest kataevidence.Resolution
	for i, entry := range slices.Backward(entries) {
		var resolution kataevidence.Resolution
		err := ctx.Err()
		if err == nil {
			resolution, err = s.Evidence.ResolveAround(ctx, entry.Reference, kataevidence.ContextRunes)
		}
		if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil && ctx.Err() != nil {
			if newest.State != "" {
				return newest, nil
			}
			return unreachable(entries[len(entries)-1]), nil
		}
		// Stop asking older entries during an outage, keeping the newest known state.
		if errors.Is(err, kataevidence.ErrDocbankUnavailable) && entry.Reference.Kind == "docbank_rendition" {
			if newest.State != "" {
				return newest, nil
			}
			return unreachable(entries[len(entries)-1]), nil
		}
		if err != nil {
			return kataevidence.Resolution{}, err
		}
		if resolution.State == kataevidence.Available {
			return resolution, nil
		}
		if i == len(entries)-1 {
			newest = resolution
			newest.Evidence.Passage = entry.Passage
		}
	}
	return newest, nil
}

// unreachable reports a passage whose Docbank transcript couldn't be read now.
func unreachable(newest Entry) kataevidence.Resolution {
	return kataevidence.Resolution{State: kataevidence.Unreachable, Evidence: kataevidence.Evidence{ID: kataevidence.ID(newest.Reference), Passage: newest.Passage,
		Reference: newest.Reference, ContentTrust: "untrusted"}}
}
