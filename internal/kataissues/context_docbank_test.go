package kataissues_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/taskclient"
)

type slowDocbank struct {
	binding    kataevidence.DocbankBinding
	reads      int
	outage     bool
	healthy    bool
	delay      time.Duration
	bindingErr error
}

type contextEvidence struct {
	*kataevidence.Service

	localErr error
}

func (f contextEvidence) ResolveAround(ctx context.Context, ref kataevidence.Reference, around int) (kataevidence.Resolution, error) {
	if ref.Kind == "docbank_rendition" {
		return f.Service.ResolveAround(ctx, ref, around)
	}
	if err := ctx.Err(); err != nil {
		return kataevidence.Resolution{}, err
	}
	if f.localErr != nil {
		return kataevidence.Resolution{}, f.localErr
	}
	return fixedText("Send email").ResolveAround(ctx, ref, around)
}

type contextKata struct {
	kataissues.Kata

	entries []kataissues.Entry
}

func (f *contextKata) GetTask(context.Context, string, string) (taskclient.KataTask, error) {
	return taskclient.KataTask{Metadata: map[string]any{kataissues.EvidenceMetadataKey: kataissues.Envelope{Version: 1, Entries: f.entries}}}, nil
}

func (f *slowDocbank) KataDocbankBinding(context.Context, string, int64) (kataevidence.DocbankBinding, error) {
	f.reads++
	if f.bindingErr != nil {
		return kataevidence.DocbankBinding{}, f.bindingErr
	}
	return f.binding, nil
}

func (f *slowDocbank) CurrentVersion(ctx context.Context, _ int64) (string, error) {
	return f.binding.ContentVersionID, f.wait(ctx)
}

func (f *slowDocbank) EvidenceIdentity(ctx context.Context, _, _, _ string) (docbankmedia.EvidenceWindowRequest, error) {
	return docbankmedia.EvidenceWindowRequest{BuildID: strings.Repeat("f", 64)}, f.wait(ctx)
}

func (f *slowDocbank) ReadEvidenceWindow(ctx context.Context, _ docbankmedia.EvidenceWindowRequest) (docbankmedia.EvidenceWindow, error) {
	if err := f.wait(ctx); err != nil {
		return docbankmedia.EvidenceWindow{}, err
	}
	if f.healthy {
		return docbankmedia.EvidenceWindow{Text: strings.Repeat("Send email", 3)}, nil
	}
	return docbankmedia.EvidenceWindow{}, &docbankmedia.HTTPError{Status: http.StatusNotFound}
}

func (f *slowDocbank) wait(ctx context.Context) error {
	if f.outage && f.reads > 1 {
		return errors.New("docbank transport: connection refused")
	}
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestContextDocbankAdmissionLimit(t *testing.T) {
	localErr := errors.New("local database failed")
	for _, tc := range []struct {
		name       string
		healthy    bool
		outage     bool
		delay      time.Duration
		attempts   int
		mixed      bool
		cancelAt   time.Duration
		localErr   error
		bindingErr error
		wantErr    error
		states     []kataevidence.State
		next       int
		elapsed    time.Duration
		reads      int
	}{
		{name: "healthy slow reads", healthy: true, delay: 3 * time.Second, attempts: 1, states: []kataevidence.State{kataevidence.Available, kataevidence.Available}, next: 2, elapsed: 6 * time.Second, reads: 2},
		{name: "stalled Docbank", delay: time.Minute, attempts: 1, states: []kataevidence.State{kataevidence.Unreachable}, next: 1, elapsed: kataevidence.DocbankTimeout, reads: 1},
		{name: "mixed local passages", delay: time.Minute, attempts: 1, mixed: true, states: []kataevidence.State{kataevidence.Unreachable, kataevidence.Available}, next: 2, elapsed: kataevidence.DocbankTimeout, reads: 1},
		{name: "outage after changed", outage: true, delay: time.Second, attempts: 20, states: []kataevidence.State{kataevidence.Changed, kataevidence.Unreachable, kataevidence.Unreachable}, elapsed: 3 * time.Second, reads: 4},
		{name: "older attempts share passage timeout", delay: time.Second, attempts: 20, states: []kataevidence.State{kataevidence.Changed}, next: 1, elapsed: kataevidence.DocbankTimeout, reads: 2},
		{name: "caller cancellation", delay: time.Minute, attempts: 1, cancelAt: time.Second, wantErr: context.Canceled, elapsed: time.Second, reads: 1},
		{name: "local error after timeout", delay: time.Minute, attempts: 1, mixed: true, localErr: localErr, wantErr: localErr, elapsed: kataevidence.DocbankTimeout, reads: 1},
		{name: "binding store failure", attempts: 1, bindingErr: localErr, wantErr: localErr, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := kataevidence.Reference{Version: kataevidence.Version, Kind: "docbank_rendition", ArchiveUID: "archive", MessageID: 4,
					SourceType: "beeper", SourceIdentifier: "signal", SourceMessageID: "voice1", AttachmentID: 9, OccurrenceKey: "msgvault:voice1"}
				docbank := &slowDocbank{binding: kataevidence.DocbankBinding{Reference: ref, VaultUID: "22222222-2222-4222-8222-222222222222",
					ContentVersionID: "11111111-1111-4111-8111-111111111111", ContentSHA256: strings.Repeat("a", 64)}, outage: tc.outage, healthy: tc.healthy, delay: tc.delay, bindingErr: tc.bindingErr}
				evidence := kataevidence.New(nil).WithDocbank(kataevidence.NewDocbankReader(docbank, docbank, func(context.Context) (string, error) { return "destination", nil }))
				var entries []kataissues.Entry
				var newest []kataissues.Entry
				for passage := range 3 {
					for i := range tc.attempts {
						ref.DocbankRendition = &kataevidence.DocbankReference{VaultUID: docbank.binding.VaultUID, NodeID: 7, ContentVersionID: docbank.binding.ContentVersionID,
							ContentSHA256: docbank.binding.ContentSHA256, RenditionAttachmentID: strings.Repeat("b", 64), BuildID: fmt.Sprintf("%064x", i),
							RenditionSHA256: strings.Repeat("d", 64), StartRune: passage * 10, EndRune: passage*10 + 10}
						canonical, err := kataevidence.Canonicalize(ref)
						require.NoError(t, err)
						entries = append(entries, kataissues.Entry{ID: kataevidence.ID(canonical), Passage: kataevidence.PassageID(canonical, "Send email"), Reference: canonical})
					}
					newest = append(newest, entries[len(entries)-1])
					if tc.mixed && passage == 0 {
						local, err := kataevidence.Canonicalize(chunkReference("archive", 4, "extraction", "a"))
						require.NoError(t, err)
						entry := kataissues.Entry{ID: kataevidence.ID(local), Passage: kataevidence.PassageID(local, "Send email"), Reference: local}
						entries = append(entries, entry)
						newest = append(newest, entry)
					}
				}
				service := &kataissues.Service{Evidence: contextEvidence{Service: evidence, localErr: tc.localErr}, Kata: &contextKata{entries: entries}, Project: "example"}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelAt > 0 {
					timer := time.AfterFunc(tc.cancelAt, cancel)
					defer timer.Stop()
				}
				start := time.Now()
				read, err := service.Context(ctx, "example-1", 0)
				assert.Equal(t, tc.elapsed, time.Since(start))
				assert.Equal(t, tc.reads, docbank.reads)
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
					if tc.bindingErr != nil {
						require.ErrorIs(t, err, kataevidence.ErrArchiveUnavailable)
						require.NotErrorIs(t, err, kataevidence.ErrDocbankUnavailable)
					}
					return
				}
				require.NoError(t, err)
				require.Len(t, read.Passages, len(tc.states))
				for i, state := range tc.states {
					assert.Equal(t, state, read.Passages[i].State)
					assert.Equal(t, newest[i].ID, read.Passages[i].Evidence.ID)
					assert.Equal(t, newest[i].Passage, read.Passages[i].Evidence.Passage)
				}
				assert.Equal(t, tc.next, read.NextOffset)
				require.NoError(t, ctx.Err())
				if tc.next > 0 {
					start = time.Now()
					continued, err := service.Context(ctx, "example-1", read.NextOffset)
					require.NoError(t, err)
					require.NotEmpty(t, continued.Passages)
					assert.Equal(t, newest[tc.next].ID, continued.Passages[0].Evidence.ID)
					assert.Equal(t, newest[tc.next].Passage, continued.Passages[0].Evidence.Passage)
					if tc.healthy {
						require.Len(t, continued.Passages, 1)
						assert.Equal(t, kataevidence.Available, continued.Passages[0].State)
						assert.Zero(t, continued.NextOffset)
						assert.Equal(t, 3*time.Second, time.Since(start))
						assert.Equal(t, 3, docbank.reads)
					}
				}
			})
		})
	}
}
