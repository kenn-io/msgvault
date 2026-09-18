package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"

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
)

func configureBeeperMediaJob(
	ctx context.Context,
	sched *scheduler.Scheduler,
	st *store.Store,
	blobs *attachmentstore.Store,
	cfg config.DocbankIntegrationConfig,
	logger *slog.Logger,
) error {
	if !cfg.Enabled {
		sched.RemoveJob(beeperMediaSubmitJob)
		err := st.UnregisterAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey)
		if errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
			return nil
		}
		return err
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	lookupKey := func() (string, error) {
		if cfg.APIKeyEnv == "" {
			return "", errors.New("docbank API key environment name is missing")
		}
		key, ok := os.LookupEnv(cfg.APIKeyEnv)
		if !ok || key == "" {
			return "", errors.New("docbank API key is unavailable")
		}
		return key, nil
	}
	client, err := docbankmedia.NewClient(endpoint, lookupKey)
	if err != nil {
		return err
	}
	archiveUID, err := st.ArchiveUIDContext(ctx)
	if err != nil {
		return err
	}
	destination := beeperMediaDestinationKey(endpoint, archiveUID)
	// Without upload consent the job only records local discovery.
	var submitClient *docbankmedia.Client
	if cfg.UploadConsent {
		submitClient = client
		if err := st.ReconsiderBlockedBeeperMediaOperations(ctx, destination); err != nil {
			return err
		}
	}
	submitter := beeper.NewMediaSubmitter(st, blobs, submitClient, destination)
	return sched.AddJob(scheduler.Job{
		Name:     beeperMediaSubmitJob,
		Schedule: beeperMediaSubmitCron,
		Run: func(ctx context.Context) error {
			result, err := submitter.RunBatch(ctx)
			if err != nil {
				return err
			}
			if logger != nil && (result.Examined > 0 || result.Journaled > 0) {
				logger.Debug("Beeper media submission pass", "examined", result.Examined,
					"journaled", result.Journaled, "pending", result.Pending,
					"retained", result.Retained, "blocked", result.Blocked)
			}
			return nil
		},
	})
}

func beeperMediaDestinationKey(endpoint, archiveUID string) string {
	digest := sha256.Sum256([]byte("beeper-media/v1\x00" + endpoint + "\x00" + archiveUID))
	return "beeper:" + hex.EncodeToString(digest[:])
}
