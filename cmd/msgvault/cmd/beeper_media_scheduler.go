package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	beeperMediaSubmitJob  = "beeper-media-submit"
	beeperMediaSubmitCron = "* * * * *"
	beeperMediaGateLabel  = "Stored media submission"
)

// beeperMediaGateWait bounds each wait for the operation gate; a busy gate ends
// the pass. Variable only so tests can shorten it.
var beeperMediaGateWait = 30 * time.Second

// beeperMediaGate takes the operation gate for one media Store step.
func beeperMediaGate(gate api.LabeledOperationGate) func(context.Context) (func(), bool) {
	if gate == nil {
		return nil
	}
	return func(ctx context.Context) (func(), bool) {
		waitCtx, cancel := context.WithTimeout(ctx, beeperMediaGateWait)
		defer cancel()
		return gate.BeginLabeledWorkContext(waitCtx, beeperMediaGateLabel)
	}
}

// withBeeperMediaGate runs a setup Store write under the operation gate.
func withBeeperMediaGate(ctx context.Context, gate api.LabeledOperationGate, write func() error) error {
	if gate == nil {
		return write()
	}
	release, ok := beeperMediaGate(gate)(ctx)
	if !ok {
		return errors.New("operation gate unavailable for Beeper media setup")
	}
	defer release()
	return write()
}

func configureBeeperMediaJob(
	ctx context.Context,
	sched *scheduler.Scheduler,
	gate api.LabeledOperationGate,
	st *store.Store,
	blobs *attachmentstore.Store,
	spoolDir string,
	cfg config.DocbankIntegrationConfig,
	logger *slog.Logger,
) error {
	if !cfg.Enabled {
		return removeBeeperMediaRoute(ctx, sched, gate, st)
	}
	err := addBeeperMediaRoute(ctx, sched, gate, st, blobs, spoolDir, cfg, logger)
	if err != nil {
		// An idle registered consumer would hold attachment change log cleanup for every provider.
		return errors.Join(err, removeBeeperMediaRoute(ctx, sched, gate, st))
	}
	return nil
}

// removeBeeperMediaRoute drops the job and its journal consumer; receipts stay.
func removeBeeperMediaRoute(ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate, st *store.Store) error {
	sched.RemoveJob(beeperMediaSubmitJob)
	err := withBeeperMediaGate(ctx, gate, func() error {
		return st.UnregisterAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey)
	})
	if errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
		return nil
	}
	return err
}

func addBeeperMediaRoute(
	ctx context.Context,
	sched *scheduler.Scheduler,
	gate api.LabeledOperationGate,
	st *store.Store,
	blobs *attachmentstore.Store,
	spoolDir string,
	cfg config.DocbankIntegrationConfig,
	logger *slog.Logger,
) error {
	client, destination, err := docbankMediaClient(ctx, st, cfg)
	if err != nil {
		return err
	}
	// Without upload consent the job only records local discovery.
	var submitClient *docbankmedia.Client
	if cfg.AllSourcesUploadConsent {
		submitClient = client
		if err := withBeeperMediaGate(ctx, gate, func() error {
			return st.ReconsiderBlockedBeeperMediaOperations(ctx, destination)
		}); err != nil {
			return err
		}
	}
	submitter := beeper.NewMediaSubmitter(st, blobs, submitClient, destination, spoolDir).
		WithASRProfile(cfg.ASRProfile).WithOperationGate(beeperMediaGate(gate))
	return sched.AddJob(scheduler.Job{
		Name:     beeperMediaSubmitJob,
		Schedule: beeperMediaSubmitCron,
		Run: func(ctx context.Context) error {
			result, err := submitter.RunBatch(ctx)
			if err != nil {
				return err
			}
			if logger != nil && (result.Examined > 0 || result.Journaled > 0) {
				logger.Debug("Stored media submission pass", "examined", result.Examined,
					"journaled", result.Journaled, "pending", result.Pending,
					"retained", result.Retained, "blocked", result.Blocked)
			}
			return nil
		},
	})
}

// docbankMediaClient builds the Docbank client and the destination key that
// the media job and the Web reader share.
func docbankMediaClient(
	ctx context.Context, st *store.Store, cfg config.DocbankIntegrationConfig,
) (*docbankmedia.Client, string, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	client, err := docbankmedia.NewClient(endpoint, cfg.ResolveAPIKey)
	if err != nil {
		return nil, "", err
	}
	archiveUID, err := st.ArchiveUIDContext(ctx)
	if err != nil {
		return nil, "", err
	}
	return client, beeperMediaDestinationKey(endpoint, archiveUID), nil
}

// newMessageRecordingReader lists archived audio even when Docbank is off.
func newMessageRecordingReader(
	ctx context.Context, st *store.Store, cfg config.DocbankIntegrationConfig,
) (*api.MessageRecordingReader, error) {
	reader := &api.MessageRecordingReader{Store: st}
	if !cfg.Enabled {
		return reader, nil
	}
	client, destination, err := docbankMediaClient(ctx, st, cfg)
	if err != nil {
		return reader, err
	}
	reader.Client, reader.Destination, reader.UploadConsent = client, destination, cfg.AllSourcesUploadConsent
	return reader, nil
}

func beeperMediaDestinationKey(endpoint, archiveUID string) string {
	digest := sha256.Sum256([]byte("beeper-media/v1\x00" + endpoint + "\x00" + archiveUID))
	return "beeper:" + hex.EncodeToString(digest[:])
}
