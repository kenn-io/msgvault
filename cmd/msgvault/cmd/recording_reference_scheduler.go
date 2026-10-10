package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/recordingref"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	recordingReferenceJob       = "recording-reference-submit"
	recordingReferenceGateLabel = "Recording reference submission"
)

func configureRecordingReferenceJob(ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate, st *store.Store, cfg config.DocbankIntegrationConfig, logger *slog.Logger) error {
	sched.RemoveJob(recordingReferenceJob)
	if !cfg.Enabled || !cfg.ReferenceConsent {
		return nil
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	client, err := docbankmedia.NewClient(endpoint, cfg.ResolveAPIKey)
	if err != nil {
		return err
	}
	var destination string
	if err := withBeeperMediaGate(ctx, gate, recordingReferenceGateLabel, func() error {
		uid, err := st.ArchiveUIDContext(ctx)
		if err != nil {
			return err
		}
		h := sha256.Sum256([]byte("recording-reference/v1\x00" + endpoint + "\x00" + uid))
		destination = "recording:" + hex.EncodeToString(h[:])
		return st.ReconsiderBlockedRecordingReferences(ctx, destination)
	}); err != nil {
		return err
	}
	worker := recordingref.NewWorker(st, client, destination, cfg.ReferenceOrigins, logger).WithOperationGate(beeperMediaGate(gate, recordingReferenceGateLabel))
	return sched.AddJob(scheduler.Job{Name: recordingReferenceJob, Schedule: "* * * * *", Run: func(ctx context.Context) error { return worker.RunBatch(ctx) }})
}
