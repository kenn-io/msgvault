package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/keepintouch"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

const keepInTouchJobName = "people-keep-in-touch"

func addKeepInTouchJob(s *scheduler.Scheduler, cfg keepintouch.Config, run func(context.Context) error) error {
	if !cfg.Enabled {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.AddJob(scheduler.Job{Name: keepInTouchJobName, Schedule: cfg.Schedule, Run: run})
}
func newKeepInTouchScheduledRun(cfg *config.Config, st *store.Store) func(context.Context) error {
	return func(ctx context.Context) error {
		service, err := newKeepInTouchService(ctx, cfg, st, false)
		if err != nil {
			return err
		}
		result, err := service.Check(ctx, false)
		return keepInTouchScheduledResult(result, err)
	}
}

type keepInTouchArchive struct {
	*store.Store

	options store.PersonTierOptions
}

func (a keepInTouchArchive) ContactStateContext(ctx context.Context, id int64, now time.Time) (store.ContactState, error) {
	return a.KeepInTouchContactStateContext(ctx, id, now, a.options)
}

// The brief's mandatory last-contact item uses the same counted coordinate as
// eligibility, rather than a newer incidental message from the broader view.
func (a keepInTouchArchive) PersonSweepLastContact(ctx context.Context, id int64) (peoplesweep.BriefLastContact, bool, error) {
	state, err := a.ContactStateContext(ctx, id, time.Now())
	if err != nil {
		return peoplesweep.BriefLastContact{}, false, err
	}
	if state.Stale || !strings.HasPrefix(state.LastContactRef, "message:") {
		return peoplesweep.BriefLastContact{}, false, nil
	}
	messageID, err := strconv.ParseInt(strings.TrimPrefix(state.LastContactRef, "message:"), 10, 64)
	if err != nil {
		return peoplesweep.BriefLastContact{}, false, fmt.Errorf("parse counted contact reference: %w", err)
	}
	sourceID := int64(0)
	if state.LastContactSourceID != nil {
		sourceID = *state.LastContactSourceID
	}
	return peoplesweep.BriefLastContact{MessageID: messageID, SourceID: sourceID, Channel: string(state.LastContactChannel)}, true, nil
}

type keepInTouchDraftFactory struct {
	cfg     *config.Config
	archive keepInTouchArchive
}

func (f keepInTouchDraftFactory) ValidateEvidence(ctx context.Context, c keepintouch.Candidate, proof []keepintouch.EvidenceSnapshot) error {
	return keepintouch.ValidateEvidence(ctx, f.archive, c.Person.ID, proof)
}
func (f keepInTouchDraftFactory) Build(ctx context.Context, c keepintouch.Candidate) (keepintouch.Draft, error) {
	profile, err := f.cfg.People.Sweep.Profile()
	if err != nil {
		return keepintouch.Draft{}, err
	}
	builder := keepintouch.ProductionDrafts{Archive: f.archive, Profile: profile, NewRunner: func() (keepintouch.TextRunner, error) { return newProductionStructuredRunner(f.cfg, f.archive.Store) }}
	return builder.Build(ctx, c)
}
func newKeepInTouchService(ctx context.Context, cfg *config.Config, st *store.Store, preview bool) (*keepintouch.Service, error) {
	if cfg == nil || st == nil {
		return nil, errors.New("keep-in-touch configuration or archive is unavailable")
	}
	archive := keepInTouchArchive{Store: st, options: store.PersonTierOptions{Config: cfg.People.Tiers, ActivityTimezone: cfg.Activity.Timezone, ActivityMaxDirect: cfg.Activity.MaxDirectCounterparts}}
	service := &keepintouch.Service{Config: cfg.People.KeepInTouch, Archive: archive, Project: cfg.Integrations.Kata.DefaultProject, Drafts: keepInTouchDraftFactory{cfg: cfg, archive: archive}}
	if !preview {
		integration := cfg.Integrations.Kata
		client, err := taskclient.ConnectKata(ctx, taskclient.IntegrationConfig{Enabled: integration.Enabled, Endpoint: integration.Endpoint, APIKey: integration.APIKey, DefaultProject: integration.DefaultProject})
		if err != nil {
			return nil, err
		}
		service.Kata = client
	}
	return service, nil
}

// Keep scheduled status actionable without logging arbitrary provider/source
// error text. Manual JSON callers retain the structured owner diagnostics.
func keepInTouchScheduledResult(result keepintouch.Result, err error) error {
	if err == nil || len(result.Failures) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, result.FailureSummary())
}
