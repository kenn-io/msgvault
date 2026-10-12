package kataevidence_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/kataevidence"
)

// fakeDocbank serves one transcript and the binding that names it. Like
// Docbank, it serves only the node's current version and transcript build.
type fakeDocbank struct {
	revoked       bool
	transcriptErr error
	statusChange  func(*docbankmedia.Receipt)
	transcripts   int
	revisionErr   error
	reads         int
	binding       kataevidence.DocbankBinding
	text          []rune
	err           error
	destination   string
	// current is the node's current version, empty once the node is deleted;
	// build is its current transcript build, empty before it is transcribed.
	current, build string
	// pruned is a version Docbank no longer holds at all.
	pruned string
	// noWindows is a Docbank older than the evidence window route.
	noWindows    bool
	windowReason string
	// finishing selects no rendition once, then one: processing finished in between.
	finishing       bool
	revision        string
	inputID         string
	origin          string
	identities      int
	transcriptBuild *string
	afterRead       func()
	// transcriptDelay is how long a transcript download takes.
	transcriptDelay time.Duration
}

func (f *fakeDocbank) MediaRevision(context.Context, int64) (string, error) {
	return f.revision, f.revisionErr
}

func (f *fakeDocbank) Status(context.Context, string) (docbankmedia.Receipt, error) {
	if f.revoked {
		return docbankmedia.Receipt{}, errNotFound
	}
	status := docbankmedia.Receipt{VaultUID: f.binding.VaultUID, SourceID: f.binding.SourceID, SourceVersionID: f.binding.SourceVersionID, ContentVersionID: f.binding.ContentVersionID, SuppliedInputID: f.binding.SuppliedInputID}
	if f.statusChange != nil {
		f.statusChange(&status)
	}
	return status, f.err
}

func (f *fakeDocbank) Transcript(ctx context.Context, _, _, _ string) (docbankmedia.MediaTranscript, error) {
	f.transcripts++
	select {
	case <-time.After(f.transcriptDelay):
	case <-ctx.Done():
		return docbankmedia.MediaTranscript{}, fmt.Errorf("docbank transport: %w", ctx.Err())
	}
	if f.transcriptErr != nil {
		return docbankmedia.MediaTranscript{}, f.transcriptErr
	}
	build := f.build
	if f.transcriptBuild != nil {
		build = *f.transcriptBuild
	}
	return docbankmedia.MediaTranscript{VaultUID: f.binding.VaultUID, EvidenceState: "ready", Transcript: &docbankmedia.MediaTranscriptEvidence{
		Origin: f.origin, BuildID: build, SuppliedInputID: f.inputID,
	}}, f.err
}

func (f *fakeDocbank) KataDocbankBinding(_ context.Context, destination string, attachmentID int64) (kataevidence.DocbankBinding, error) {
	f.destination = destination
	if attachmentID != f.binding.Reference.AttachmentID {
		return kataevidence.DocbankBinding{}, kataevidence.ErrUnavailable
	}
	return f.binding, nil
}

func (f *fakeDocbank) CurrentVersion(context.Context, int64) (string, error) {
	if f.current == "" {
		return "", docbankmedia.ErrEvidenceUnavailable
	}
	return f.current, f.err
}

var errNotFound = &docbankmedia.HTTPError{Status: http.StatusNotFound, Code: "not_found"}

func (f *fakeDocbank) EvidenceIdentity(_ context.Context, contentVersionID, contentSHA256, profile string) (docbankmedia.EvidenceWindowRequest, error) {
	f.identities++
	if f.err != nil {
		return docbankmedia.EvidenceWindowRequest{}, f.err
	}
	if f.finishing {
		f.finishing = false
		return docbankmedia.EvidenceWindowRequest{NodeID: 7}, docbankmedia.ErrNoRendition
	}
	if contentVersionID == f.pruned {
		return docbankmedia.EvidenceWindowRequest{}, docbankmedia.ErrEvidenceUnavailable
	}
	if contentVersionID != f.current || f.build == "" {
		return docbankmedia.EvidenceWindowRequest{NodeID: 7}, docbankmedia.ErrNoRendition
	}
	return docbankmedia.EvidenceWindowRequest{NodeID: 7, ContentVersionID: contentVersionID, ContentSHA256: contentSHA256,
		RenditionAttachmentID: strings.Repeat("b", 64), BuildID: f.build, RenditionSHA256: strings.Repeat("d", 64)}, nil
}

func (f *fakeDocbank) ReadEvidenceWindow(ctx context.Context, request docbankmedia.EvidenceWindowRequest) (docbankmedia.EvidenceWindow, error) {
	f.reads++
	if ctx.Err() != nil {
		return docbankmedia.EvidenceWindow{}, fmt.Errorf("docbank transport: %w", ctx.Err())
	}
	if f.err != nil {
		return docbankmedia.EvidenceWindow{}, f.err
	}
	if f.noWindows {
		return docbankmedia.EvidenceWindow{}, &docbankmedia.HTTPError{Status: http.StatusNotFound, Code: "not_found", Reason: f.windowReason}
	}
	if request.ContentVersionID != f.current || request.BuildID != f.build {
		return docbankmedia.EvidenceWindow{}, errNotFound
	}
	if request.Offset > len(f.text) {
		return docbankmedia.EvidenceWindow{}, &docbankmedia.HTTPError{Status: http.StatusRequestedRangeNotSatisfiable}
	}
	text := f.text[request.Offset:min(len(f.text), request.Offset+request.MaxChars)]
	end := request.Offset + len(text)
	if f.afterRead != nil {
		f.afterRead()
	}
	return docbankmedia.EvidenceWindow{Text: string(text), ActualStart: request.Offset, ActualEnd: end, EOF: end == len(f.text)}, nil
}

func newFakeDocbank() *fakeDocbank {
	return &fakeDocbank{
		binding: kataevidence.DocbankBinding{
			Revision: "revision", SourceID: "source", SourceVersionID: "version", SuppliedInputID: "input",
			Reference: kataevidence.Reference{Version: kataevidence.Version, Kind: "docbank_rendition", ArchiveUID: "archive", MessageID: 4,
				SourceType: "beeper", SourceIdentifier: "signal", SourceMessageID: "voice1", AttachmentID: 9, OccurrenceKey: "msgvault:voice1"},
			VaultUID: "22222222-2222-4222-8222-222222222222", ContentVersionID: "11111111-1111-4111-8111-111111111111", ContentSHA256: strings.Repeat("a", 64), Profile: "supplied-transcript",
			Display: kataevidence.Display{Filename: "voice.wav", ContainingTitle: "Budget review"},
		},
		text:     []rune(strings.Repeat("é", kataevidence.MaxChars) + "Send the revised budget by Friday."),
		current:  "11111111-1111-4111-8111-111111111111",
		build:    strings.Repeat("c", 64),
		revision: "revision", inputID: "input",
		origin: "supplied",
	}
}

func TestDocbankRejectsStaleTranscriptProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeDocbank)
		want   error
	}{
		{"revision callback changed", func(f *fakeDocbank) { f.revisionErr = kataevidence.ErrChanged }, kataevidence.ErrChanged},
		{"revision callback unavailable", func(f *fakeDocbank) { f.revisionErr = kataevidence.ErrUnavailable }, kataevidence.ErrUnavailable},
		{"revision database error", func(f *fakeDocbank) { f.revisionErr = kataevidence.ErrArchiveUnavailable }, kataevidence.ErrArchiveUnavailable},
		{"source edited before read", func(f *fakeDocbank) { f.revision = "edited" }, kataevidence.ErrChanged},
		{"source edited during read", func(f *fakeDocbank) { f.afterRead = func() { f.revision = "edited" } }, kataevidence.ErrChanged},
		{"shared audio with another input", func(f *fakeDocbank) { f.inputID = "other-message-input" }, kataevidence.ErrUnavailable},
		{"missing input", func(f *fakeDocbank) { f.inputID = "" }, kataevidence.ErrUnavailable},
		{"ready without build", func(f *fakeDocbank) { f.transcriptBuild = new("") }, kataevidence.ErrUnsupported},
		{"another build", func(f *fakeDocbank) { f.transcriptBuild = new(strings.Repeat("e", 64)) }, kataevidence.ErrChanged},
		{"build changed during read", func(f *fakeDocbank) { f.afterRead = func() { f.build = strings.Repeat("e", 64) } }, kataevidence.ErrChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			for _, resolve := range []bool{false, true} {
				f := newFakeDocbank()
				reader := kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
				selector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(10)}
				prepared, err := reader.LoadKataEvidenceSource(t.Context(), selector)
				require.NoError(err)
				tc.change(f)
				reader = kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
				var result kataevidence.SourceRecord
				if resolve {
					prepared.Reference.DocbankRendition.EndRune = 10
					result, err = reader.ReadKataEvidenceSource(t.Context(), prepared.Reference)
				} else {
					result, err = reader.LoadKataEvidenceSource(t.Context(), selector)
				}
				require.ErrorIs(err, tc.want, "resolve=%v", resolve)
				assert.Empty(result.Text)
				assert.Equal(f.binding.Display, result.Display)
			}
		})
	}
}

func TestDocbankWindowNotFoundReason(t *testing.T) {
	for reason, want := range map[string]error{"": kataevidence.ErrUnsupported, "evidence_unavailable": kataevidence.ErrUnavailable} {
		t.Run(reason, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeDocbank()
			reader := kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
			selector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(10)}
			prepared, err := reader.LoadKataEvidenceSource(t.Context(), selector)
			require.NoError(err)
			prepared.Reference.DocbankRendition.EndRune = 10
			f.noWindows, f.windowReason = true, reason
			result, err := reader.LoadKataEvidenceSource(t.Context(), selector)
			require.ErrorIs(err, want)
			assert.Empty(result.Text)
			result, err = reader.ReadKataEvidenceSource(t.Context(), prepared.Reference)
			require.ErrorIs(err, want)
			assert.Empty(result.Text)
			f.build = strings.Repeat("e", 64)
			result, err = reader.ReadKataEvidenceSource(t.Context(), prepared.Reference)
			require.ErrorIs(err, kataevidence.ErrChanged)
			assert.Empty(result.Text)
			assert.Equal(f.binding.Display, result.Display)
		})
	}
}

func TestDocbankSlowTranscriptCheckFinishesForRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newFakeDocbank()
		f.transcriptDelay = 3 * kataevidence.DocbankTimeout
		reader := kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
		selector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(10)}
		for range 2 {
			_, err := reader.LoadKataEvidenceSource(t.Context(), selector)
			require.ErrorIs(err, kataevidence.ErrDocbankUnavailable, "a timed-out check is worth retrying")
		}
		time.Sleep(f.transcriptDelay)
		synctest.Wait()
		prepared, err := reader.LoadKataEvidenceSource(t.Context(), selector)
		require.NoError(err)
		assert.Equal(string(f.text[:10]), prepared.Text)
		assert.Equal(1, f.transcripts, "timed-out attempts share one download, and the retry reuses its result")
	})
}

func TestDocbankGeneratedTranscriptProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeDocbank()
	f.origin, f.inputID, f.binding.SuppliedInputID, f.binding.Profile = "generated", "", "", "audio-transcript"
	reader := kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
	selector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(10)}
	prepared, err := reader.LoadKataEvidenceSource(t.Context(), selector)
	require.NoError(err)
	assert.Equal(1, f.identities)
	prepared.Reference.DocbankRendition.EndRune = 10
	resolved, err := kataevidence.New(nil).WithDocbank(reader).ResolveAround(t.Context(), prepared.Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Available, resolved.State)
	assert.Equal(prepared.Text, resolved.Evidence.Excerpt)
	assert.Equal(1, f.identities)
	assert.Equal(1, f.transcripts, "the same pinned build reuses verified metadata")
	f.inputID = "unexpected-supplied-input"
	reader = kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
	_, err = reader.ReadKataEvidenceSource(t.Context(), prepared.Reference)
	require.ErrorIs(err, kataevidence.ErrUnavailable)
	// A supplied-transcript delivery can't cite a generated transcript.
	f.inputID, f.binding.SuppliedInputID, f.binding.Profile = "", "input", "supplied-transcript"
	_, err = reader.LoadKataEvidenceSource(t.Context(), selector)
	require.ErrorIs(err, kataevidence.ErrUnavailable)
}

func TestDocbankWindowsPageAndResolve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	docbank := newFakeDocbank()
	svc := kataevidence.New(nil).WithDocbank(kataevidence.NewDocbankReader(docbank, docbank, func(context.Context) (string, error) { return "destination", nil }, docbank.MediaRevision))
	selector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(kataevidence.MaxChars)}

	first, err := svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.NoError(err)
	assert.Equal("destination", docbank.destination)
	assert.Equal(strings.Repeat("é", kataevidence.MaxChars), first[0].Excerpt)
	assert.Equal(kataevidence.MaxChars, first[0].NextRune, "a full window says where the next one starts")
	assert.Equal("voice.wav", first[0].Display.Filename)

	selector.StartRune = &first[0].NextRune
	last, err := svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.NoError(err)
	assert.Equal("Send the revised budget by Friday.", last[0].Excerpt)
	assert.Zero(last[0].NextRune, "the last window has no following passage")
	ref := last[0].Reference
	require.NotNil(ref.DocbankRendition)
	assert.Equal(kataevidence.DocbankReference{VaultUID: "22222222-2222-4222-8222-222222222222", NodeID: 7, ContentVersionID: "11111111-1111-4111-8111-111111111111", ContentSHA256: strings.Repeat("a", 64),
		RenditionAttachmentID: strings.Repeat("b", 64), BuildID: strings.Repeat("c", 64), RenditionSHA256: strings.Repeat("d", 64),
		StartRune: kataevidence.MaxChars, EndRune: kataevidence.MaxChars + 34}, *ref.DocbankRendition)

	resolved, err := svc.ResolveAround(t.Context(), ref, kataevidence.ContextRunes)
	require.NoError(err)
	assert.Equal(kataevidence.Available, resolved.State)
	assert.Equal(last[0].Excerpt, resolved.Evidence.Excerpt)
	assert.Equal(last[0].Passage, resolved.Evidence.Passage)
	assert.Equal(strings.Repeat("é", kataevidence.ContextRunes), resolved.Before)

	unconfigured := kataevidence.New(nil)
	_, err = unconfigured.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnsupported)
	resolved, err = unconfigured.ResolveAround(t.Context(), ref, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Unsupported, resolved.State)

	reads := docbank.reads
	limitSelector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(1)}
	duplicates := slices.Repeat([]kataevidence.Selector{limitSelector}, kataevidence.MaxDocbankReferences+1)
	prepared, err := svc.Prepare(t.Context(), duplicates)
	require.NoError(err)
	assert.Len(prepared, len(duplicates))
	assert.Equal(reads+1, docbank.reads)
	for i := range duplicates {
		duplicates[i].StartRune = new(i)
	}
	_, err = svc.Prepare(t.Context(), duplicates[:kataevidence.MaxDocbankReferences])
	require.NoError(err)
	assert.Equal(reads+1+kataevidence.MaxDocbankReferences, docbank.reads)
	_, err = svc.Prepare(t.Context(), duplicates)
	require.ErrorIs(err, kataevidence.ErrDocbankLimit)
	assert.Equal(reads+1+kataevidence.MaxDocbankReferences, docbank.reads, "over-limit requests never read Docbank")

	text := docbank.text
	docbank.text = text[:1]
	resolved, err = svc.ResolveAround(t.Context(), ref, kataevidence.ContextRunes)
	require.NoError(err)
	assert.Equal(kataevidence.Unavailable, resolved.State, "the cited window starts past the transcript's end")
	docbank.text = text

	docbank.build = strings.Repeat("e", 64)
	resolved, err = svc.ResolveAround(t.Context(), ref, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Changed, resolved.State, "Docbank transcribed the file again")
	assert.Equal("Budget review", resolved.Evidence.Display.ContainingTitle)

	docbank.build, docbank.current = strings.Repeat("c", 64), "33333333-3333-4333-8333-333333333333"
	resolved, err = svc.ResolveAround(t.Context(), ref, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Changed, resolved.State, "Docbank holds a newer version of the file")
	assert.Equal("Budget review", resolved.Evidence.Display.ContainingTitle)

	docbank.current = ""
	resolved, err = svc.ResolveAround(t.Context(), ref, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Unavailable, resolved.State, "Docbank deleted the file")

	docbank.current = "11111111-1111-4111-8111-111111111111"

	// Preparing finds the same states, from the file's current version.
	for current, want := range map[string]error{"33333333-3333-4333-8333-333333333333": kataevidence.ErrChanged} {
		docbank.current = current
		_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
		require.ErrorIs(err, want, current)
	}
	docbank.current, docbank.build = "11111111-1111-4111-8111-111111111111", ""
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnprocessed, "Docbank has not transcribed the file yet")
	docbank.build, docbank.finishing = strings.Repeat("c", 64), true
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnprocessed, "the select found no transcript, whatever a later read sees")
	docbank.build, docbank.current, docbank.pruned = strings.Repeat("c", 64), "33333333-3333-4333-8333-333333333333", "11111111-1111-4111-8111-111111111111"
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnavailable, "Docbank pruned the version, so retrying won't help")
	docbank.current, docbank.pruned = "11111111-1111-4111-8111-111111111111", ""
	docbank.noWindows = true
	resolved, err = svc.ResolveAround(t.Context(), ref, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Unsupported, resolved.State, "Docbank is too old to serve windows")
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnsupported)
	docbank.noWindows = false

	// An outage is worth retrying, so create and link answer 503 rather than 404.
	docbank.err = errors.New("docbank transport: connection refused")
	_, err = svc.ResolveAround(t.Context(), ref, 0)
	require.ErrorIs(err, kataevidence.ErrArchiveUnavailable)
	require.ErrorIs(err, kataevidence.ErrDocbankUnavailable)
	docbank.err = &docbankmedia.HTTPError{Status: http.StatusBadGateway, Code: "server_error"}
	_, err = svc.ResolveAround(t.Context(), ref, 0)
	require.ErrorIs(err, kataevidence.ErrArchiveUnavailable)
	require.ErrorIs(err, kataevidence.ErrDocbankUnavailable)
	// Only an outage is retryable; a missing transcript file or a refused request isn't.
	for _, tc := range []struct {
		err  *docbankmedia.HTTPError
		want kataevidence.State
	}{
		{&docbankmedia.HTTPError{Status: http.StatusConflict, Code: "conflict", Reason: "rendition_selection_stale"}, kataevidence.Changed},
		{&docbankmedia.HTTPError{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Reason: "rendition_text_too_large"}, kataevidence.Unavailable},
		{&docbankmedia.HTTPError{Status: http.StatusServiceUnavailable, Code: "server_error", Reason: "content_missing"}, kataevidence.Unavailable},
		{&docbankmedia.HTTPError{Status: http.StatusBadRequest, Code: "bad_request"}, kataevidence.Unsupported},
		{&docbankmedia.HTTPError{Status: http.StatusUnprocessableEntity, Code: "validation", Reason: "processing_profile_unavailable"}, kataevidence.Unsupported},
		{&docbankmedia.HTTPError{Status: http.StatusUnprocessableEntity, Reason: "rendition_failed"}, kataevidence.Failed},
		{&docbankmedia.HTTPError{Status: http.StatusForbidden, Code: "forbidden"}, kataevidence.Unsupported},
	} {
		docbank.err = tc.err
		resolved, err = svc.ResolveAround(t.Context(), ref, 0)
		require.NoError(err)
		assert.Equal(tc.want, resolved.State, tc.err.Code)
	}
	docbank.err = docbankmedia.ErrCredentialUnavailable
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrUnsupported, "a missing credential")
	docbank.err = errors.New("docbank transport: connection refused")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = svc.ResolveAround(canceled, ref, 0)
	require.ErrorIs(err, context.Canceled, "the caller's own cancellation stays an error")
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{selector})
	require.ErrorIs(err, kataevidence.ErrArchiveUnavailable, "preparing against an unreachable Docbank is worth retrying")

	// msgvault reads a transcript a window at a time, so it can't find a quote in it.
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, Quote: "Send"}})
	require.ErrorIs(err, kataevidence.ErrInvalidReference)
}

func TestDocbankRechecksVisibilityAndColdAbsence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*fakeDocbank)
		wantErr error
		want    kataevidence.State
	}{
		{"revoked", func(f *fakeDocbank) { f.revoked = true }, kataevidence.ErrUnavailable, kataevidence.Unavailable},
		{"transcript 404", func(f *fakeDocbank) { f.transcriptErr = errNotFound }, kataevidence.ErrUnavailable, kataevidence.Unavailable},
		{"vault changed", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.VaultUID = "other" } }, kataevidence.ErrUnavailable, kataevidence.Unavailable},
		{"source changed", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.SourceID = "other" } }, kataevidence.ErrUnavailable, kataevidence.Unavailable},
		{"source version changed", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.SourceVersionID = "other" } }, kataevidence.ErrChanged, kataevidence.Changed},
		{"content version changed", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.ContentVersionID = "other" } }, kataevidence.ErrChanged, kataevidence.Changed},
		{"input revoked", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.CoverageState = "stale" } }, kataevidence.ErrUnavailable, kataevidence.Unavailable},
		{"input withdrawn", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.SuppliedInputID = "" } }, kataevidence.ErrUnavailable, kataevidence.Unavailable},
		{"input changed", func(f *fakeDocbank) { f.statusChange = func(s *docbankmedia.Receipt) { s.SuppliedInputID = "other" } }, kataevidence.ErrChanged, kataevidence.Changed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeDocbank()
			reader := kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
			selector := kataevidence.Selector{Kind: "docbank_rendition", MessageID: 4, AttachmentID: 9, MaxChars: new(10)}
			prepared, err := reader.LoadKataEvidenceSource(t.Context(), selector)
			require.NoError(err)
			prepared.Reference.DocbankRendition.EndRune = 10
			tc.change(f)
			if tc.name == "transcript 404" {
				reader = kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
			}
			_, err = reader.LoadKataEvidenceSource(t.Context(), selector)
			require.ErrorIs(err, tc.wantErr)
			resolved, err := kataevidence.New(nil).WithDocbank(reader).ResolveAround(t.Context(), prepared.Reference, 0)
			require.NoError(err)
			assert.Equal(tc.want, resolved.State)
			cold := kataevidence.NewDocbankReader(f, f, func(context.Context) (string, error) { return "destination", nil }, f.MediaRevision)
			_, err = cold.ReadKataEvidenceSource(t.Context(), prepared.Reference)
			require.ErrorIs(err, tc.wantErr)
		})
	}
}
