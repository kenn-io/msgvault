package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/tiktokwebhook"
)

func TestTikTokMessageCommandGrammarAndCache(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root := &cobra.Command{Use: "msgvault"}
	source := &cobra.Command{Use: "source"}
	root.AddCommand(source)
	source.AddCommand(newTikTokMessagesCmd())
	for _, action := range []string{"add", "sync", "status", "replay"} {
		cmd, args, err := root.Find([]string{"source", "tiktok-messages", action, "example"})
		requirements.NoError(err)
		assertions.Equal(action, cmd.Name())
		forwarded, err := daemonCLIArgsFromCobra(cmd, args)
		requirements.NoError(err)
		assertions.Equal([]string{"source", "tiktok-messages", action, "example"}, forwarded)
		assertions.Equal(action == "sync" || action == "add", manualSyncCLICommand(forwarded))
	}
}

func TestTikTokDeliveryStatusAndReplay(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.TikTok = []config.TikTokSource{{Identifier: "example", AppID: "app-example", AccountID: "account-example", Enabled: true, MessagesEnabled: true}}
	spool, err := tiktokwebhook.OpenSpool(tikTokSpoolRoot(cfg), 1<<20)
	requirements.NoError(err)
	_, _, err = spool.Accept(t.Context(), "app-example", []byte("synthetic private content"))
	requirements.NoError(err)
	_, err = spool.Drain(t.Context(), func(context.Context, string, []byte) (tiktokwebhook.Outcome, error) {
		return tiktokwebhook.Outcome{Class: "unrouted"}, nil
	})
	requirements.NoError(err)
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	var output bytes.Buffer
	cmd.SetOut(&output)
	requirements.NoError(runTikTokDeliveryControl(cmd, cfg, []string{"example"}, "status", true))
	var status struct {
		Deliveries tiktokwebhook.Status `json:"deliveries"`
	}
	requirements.NoError(json.Unmarshal(output.Bytes(), &status))
	assertions.Equal(1, status.Deliveries.Held)
	assertions.NotContains(output.String(), "synthetic private content")
	output.Reset()
	requirements.NoError(runTikTokDeliveryControl(cmd, cfg, []string{"example"}, "replay", true))
	assertions.JSONEq(`{"requeued":1}`, output.String())
}

func TestTikTokRuntimeRecoversSpoolAndStops(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.TikTokWebhooks = config.TikTokWebhooksConfig{ListenAddr: "127.0.0.1:0", Apps: []config.TikTokWebhookApp{{AppID: "app-example", AppSecretEnv: "EXAMPLE_TIKTOK_SECRET"}}}
	t.Setenv("EXAMPLE_TIKTOK_SECRET", "fixture-app-secret")
	spool, err := tiktokwebhook.OpenSpool(tikTokSpoolRoot(cfg), 1<<20)
	requirements.NoError(err)
	body := []byte("synthetic accepted delivery")
	_, _, err = spool.Accept(t.Context(), "app-example", body)
	requirements.NoError(err)
	processed := make(chan []byte, 1)
	r, err := startTikTokWebhooksWithHandler(t.Context(), cfg, nil, func(_ context.Context, _ string, raw []byte) (tiktokwebhook.Outcome, error) {
		processed <- raw
		return tiktokwebhook.Outcome{Class: "applied"}, nil
	})
	requirements.NoError(err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		assertions.NoError(r.Shutdown(ctx))
	})
	select {
	case raw := <-processed:
		assertions.Equal(body, raw)
	case <-time.After(15 * time.Second):
		requirements.FailNow("accepted delivery did not reach the recovered worker")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+r.server.Addr+"/webhooks/tiktok/app-example", nil)
	requirements.NoError(err)
	resp, err := client.Do(req)
	requirements.NoError(err)
	assertions.Equal(http.StatusMethodNotAllowed, resp.StatusCode)
	requirements.NoError(resp.Body.Close())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	requirements.NoError(r.Shutdown(ctx))
	assertions.Equal("stopped", r.snapshot().Listener)
	// Restarting without receiver configuration must replace the old listener
	// snapshot so status cannot keep reporting a previously configured listener.
	cfg.TikTokWebhooks = config.TikTokWebhooksConfig{}
	_, err = startTikTokWebhooksWithHandler(t.Context(), cfg, nil, nil)
	requirements.NoError(err)
	var statusOutput bytes.Buffer
	statusCommand := &cobra.Command{Use: "status"}
	statusCommand.SetContext(t.Context())
	statusCommand.SetOut(&statusOutput)
	requirements.NoError(runTikTokDeliveryControl(statusCommand, cfg, nil, "status", true))
	var status struct {
		Receiver tikTokReceiverSnapshot `json:"receiver"`
	}
	requirements.NoError(json.Unmarshal(statusOutput.Bytes(), &status))
	assertions.Equal("disabled", status.Receiver.Listener)
}

func TestTikTokDeliveryCoordinatesArchiveAndAttachmentMaintenance(t *testing.T) {
	for _, held := range []string{"archive gate", "attachment maintenance"} {
		t.Run(held, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				gate := api.NewSerialOperationGate()
				maintenance := &attachmentMaintenance{coordinator: packstore.NewCoordinator()}
				var release func()
				if held == "archive gate" {
					done, ok := gate.BeginWorkContext(t.Context())
					requirements.True(ok)
					release = done
				} else {
					lease, err := maintenance.coordinator.AcquireMaintenance(t.Context())
					requirements.NoError(err)
					release = func() { assertions.NoError(lease.Release()) }
				}
				releaseOnce := sync.OnceFunc(release)
				t.Cleanup(releaseOnce)
				calls := 0
				handle := coordinateTikTokDeliveries(gate, maintenance, func(context.Context, string, []byte) (tiktokwebhook.Outcome, error) {
					calls++
					return tiktokwebhook.Outcome{Class: "applied"}, nil
				})
				// Cancellation bounds the wait for an occupied real gate or lease.
				// Ingest must not begin while either maintenance boundary is held.
				ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
				defer cancel()
				_, err := handle(ctx, "app-example", []byte("synthetic delivery"))
				requirements.ErrorIs(err, context.DeadlineExceeded)
				assertions.Zero(calls)
				releaseOnce()
				outcome, err := handle(t.Context(), "app-example", []byte("synthetic delivery"))
				requirements.NoError(err)
				assertions.Equal("applied", outcome.Class)
				assertions.Equal(1, calls)
			})
		})
	}
}

func TestTikTokAttachmentWritingCommands(t *testing.T) {
	assertions := assert.New(t)
	for _, operation := range []string{"add", "sync"} {
		assertions.True(attachmentProducingCommand([]string{"source", "tiktok-messages", operation, "example"}))
	}
	for _, operation := range []string{"status", "replay"} {
		assertions.False(attachmentProducingCommand([]string{"source", "tiktok-messages", operation, "example"}))
	}
	assertions.False(attachmentProducingCommand([]string{"source", "tiktok-comments", "sync", "example"}))
}
