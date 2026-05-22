package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestServeConfigParsing(t *testing.T) {
	// Create temp config with scheduled accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"

[server]
api_port = 9090
api_key = "test-key"

[[accounts]]
email = "user1@gmail.com"
schedule = "0 2 * * *"
enabled = true

[[accounts]]
email = "user2@gmail.com"
schedule = "0 3 * * *"
enabled = true

[[accounts]]
email = "disabled@gmail.com"
schedule = "0 4 * * *"
enabled = false
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify server config
	if cfg.Server.APIPort != 9090 {
		t.Errorf("APIPort = %d, want 9090", cfg.Server.APIPort)
	}
	if cfg.Server.APIKey != "test-key" {
		t.Errorf("APIKey = %q, want test-key", cfg.Server.APIKey)
	}

	// Verify scheduled accounts
	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) != 2 {
		t.Errorf("len(ScheduledAccounts()) = %d, want 2", len(scheduled))
	}

	// Verify specific accounts
	acc := cfg.GetAccountSchedule("user1@gmail.com")
	if acc == nil {
		t.Fatal("GetAccountSchedule(user1) = nil")
	}
	if acc.Schedule != "0 2 * * *" {
		t.Errorf("user1 schedule = %q, want '0 2 * * *'", acc.Schedule)
	}

	// Disabled account should still be retrievable but not in scheduled list
	disabled := cfg.GetAccountSchedule("disabled@gmail.com")
	if disabled == nil {
		t.Error("GetAccountSchedule(disabled) = nil, want non-nil")
	}
}

func TestSchedulerWithConfig(t *testing.T) {
	cfg := &config.Config{
		Accounts: []config.AccountSchedule{
			{Email: "test1@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "test2@gmail.com", Schedule: "0 3 * * *", Enabled: true},
			{Email: "test3@gmail.com", Schedule: "invalid", Enabled: true},
		},
	}

	var syncCalls []string
	sched := scheduler.New(func(ctx context.Context, email string) error {
		syncCalls = append(syncCalls, email)
		return nil
	})

	count, errs := sched.AddAccountsFromConfig(cfg)

	// Should schedule 2 valid accounts
	if count != 2 {
		t.Errorf("scheduled = %d, want 2", count)
	}

	// Should have 1 error for invalid cron
	if len(errs) != 1 {
		t.Errorf("len(errs) = %d, want 1", len(errs))
	}

	// Verify status
	statuses := sched.Status()
	if len(statuses) != 2 {
		t.Errorf("len(Status()) = %d, want 2", len(statuses))
	}
}

func TestServeCmdNoAccounts(t *testing.T) {
	// Create temp config without accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) != 0 {
		t.Errorf("expected no scheduled accounts, got %d", len(scheduled))
	}
}

// TestSetupVectorFeatures_Disabled verifies that when
// cfg.Vector.Enabled is false, setupVectorFeatures returns (nil, nil)
// regardless of build tag. Runs under both tagged and untagged builds.
func TestSetupVectorFeatures_Disabled(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Vector.Enabled = false

	vf, err := setupVectorFeatures(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("setupVectorFeatures error = %v, want nil", err)
	}
	if vf != nil {
		t.Errorf("setupVectorFeatures = %v, want nil when disabled", vf)
	}
}

func TestCronExpressionValidation(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"daily at 2am", "0 2 * * *", false},
		{"every 15 min", "*/15 * * * *", false},
		{"weekly sunday", "0 0 * * 0", false},
		{"monthly first", "0 0 1 * *", false},
		{"twice daily", "0 8,18 * * *", false},
		{"invalid", "not a cron", true},
		{"empty", "", true},
		{"too many fields", "* * * * * *", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scheduler.ValidateCronExpr(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCronExpr(%q) error = %v, wantErr = %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}
