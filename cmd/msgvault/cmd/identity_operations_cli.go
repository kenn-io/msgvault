package cmd

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func init() { identityCmd.AddCommand(newIdentityOperationsCmd()) }

type identityCLIRequest struct {
	Operation           identitycontrol.Operation      `json:"operation"`
	Target              identitycontrol.IdentityTarget `json:"target"`
	ExpectedFingerprint string                         `json:"expected_fingerprint"`
	PreviewToken        string                         `json:"preview_token"`
	IdempotencyKey      string                         `json:"idempotency_key"`
}

func newIdentityOperationsCmd() *cobra.Command {
	parent := &cobra.Command{Use: "operations", Short: "Preview, apply and reconcile one exact native identity change", Long: "Link or unlink a verified participant pair, or attach or detach a participant from a durable person. Commands preview by default. Use --apply with the exact signed preview, expected fingerprint and idempotency key to write through the daemon."}
	for _, operation := range []identitycontrol.Operation{identitycontrol.OperationGraphLink, identitycontrol.OperationGraphUnlink, identitycontrol.OperationPersonLink, identitycontrol.OperationPersonUnlink} {
		var path string
		var apply bool
		command := &cobra.Command{Use: string(operation), Short: "Preview one " + string(operation) + "; execute only with --apply", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := readIdentityCLIRequest(cmd, path)
			if err != nil {
				return usageErr(cmd, err)
			}
			if request.Operation != "" && request.Operation != operation {
				return usageErr(cmd, fmt.Errorf("%w: request operation differs from command", identitycontrol.ErrInvalidRequest))
			}
			intent := identitycontrol.PreviewRequest{Operation: operation, Target: request.Target}
			if err := intent.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			body := generated.IdentityOperationApplyRequest{Operation: generated.IdentityOperationApplyRequestOperation(operation), Target: identityCLITarget(request.Target), ExpectedFingerprint: request.ExpectedFingerprint, PreviewToken: request.PreviewToken, IdempotencyKey: request.IdempotencyKey}
			if apply {
				if err := body.Validate(); err != nil {
					return usageErr(cmd, fmt.Errorf("%w: apply requires a fingerprint, signed preview and idempotency key", identitycontrol.ErrInvalidRequest))
				}
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			if !apply {
				preview, err := client.PreviewIdentityOperation(cmd.Context(), intent)
				if err != nil {
					return err
				}
				return writeIdentityCLIResult(cmd, preview)
			}
			receipt, err := client.ApplyIdentityOperation(cmd.Context(), body)
			if _, unknown := errors.AsType[*daemonclient.IdentityOutcomeUnknownError](err); unknown {
				return fmt.Errorf("original idempotency key %q; read identity operations receipt before retrying: %w", request.IdempotencyKey, err)
			}
			if err != nil {
				return err
			}
			if err := writeIdentityCLIResult(cmd, receipt); err != nil {
				return fmt.Errorf("original idempotency key %q; read identity operations receipt before retrying: %w", request.IdempotencyKey, &daemonclient.IdentityOutcomeUnknownError{IdempotencyKey: request.IdempotencyKey, Cause: err})
			}
			return nil
		}}
		command.Flags().StringVar(&path, "request", "", "JSON request file, or - for standard input (maximum 16 KiB)")
		command.Flags().BoolVar(&apply, "apply", false, "Execute with the signed native preview, expected fingerprint and idempotency key")
		_ = command.MarkFlagRequired("request")
		parent.AddCommand(command)
	}
	parent.AddCommand(newIdentityReceiptCmd())
	return parent
}

func identityCLITarget(target identitycontrol.IdentityTarget) generated.IdentityTarget {
	result := generated.IdentityTarget{ParticipantID: target.ParticipantID}
	if target.OtherParticipantID != 0 {
		result.OtherParticipantID = new(target.OtherParticipantID)
	}
	if target.PersonID != 0 {
		result.PersonID = new(target.PersonID)
	}
	return result
}

func readIdentityCLIRequest(cmd *cobra.Command, path string) (identityCLIRequest, error) {
	var request identityCLIRequest
	reader := cmd.InOrStdin()
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return request, fmt.Errorf("open identity request: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, 16*1024+1))
	if err != nil {
		return request, fmt.Errorf("read identity request: %w", err)
	}
	if len(data) > 16*1024 || len(data) == 0 || json.Unmarshal(data, &request, json.RejectUnknownMembers(true)) != nil {
		return request, fmt.Errorf("%w: a canonical JSON request object up to 16 KiB is required", identitycontrol.ErrInvalidRequest)
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || token.Kind() == 'n' {
			return request, fmt.Errorf("%w: null fields are not permitted", identitycontrol.ErrInvalidRequest)
		}
	}
	return request, nil
}

func newIdentityReceiptCmd() *cobra.Command {
	var key, id, principal string
	command := &cobra.Command{Use: "receipt", Short: "Read a committed native identity outcome without replaying its mutation", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if (key == "") == (id == "") || id != "" && principal != "" {
			return usageErr(cmd, fmt.Errorf("%w: select a key or receipt ID; principal requires a key", identitycontrol.ErrInvalidRequest))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		var receipt *generated.IdentityReceipt
		switch {
		case id != "":
			receipt, err = client.GetIdentityOperationReceiptByID(cmd.Context(), id)
		case principal != "":
			receipt, err = client.GetIdentityOperationReceiptForPrincipal(cmd.Context(), principal, key)
		default:
			receipt, err = client.GetIdentityOperationReceipt(cmd.Context(), key)
		}
		if err != nil {
			return err
		}
		return writeIdentityCLIResult(cmd, receipt)
	}}
	command.Flags().StringVar(&key, "idempotency-key", "", "Original request key for the current principal")
	command.Flags().StringVar(&id, "receipt-id", "", "Exact receipt ID (owner recovery only)")
	command.Flags().StringVar(&principal, "principal", "", "Prior principal with its original key (owner recovery only)")
	return command
}

func writeIdentityCLIResult(cmd *cobra.Command, value any) error {
	data, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("encode identity result: %w", err)
	}
	// A committed result must return EPIPE so callers can explain receipt recovery.
	// Stop restores the prior signal behavior and preserves other subscribers.
	brokenPipe := make(chan os.Signal, 1)
	signal.Notify(brokenPipe, syscall.SIGPIPE)
	defer signal.Stop(brokenPipe)
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
		return fmt.Errorf("write identity result: %w", err)
	}
	return nil
}

func isIdentityOperationCommand(cmd *cobra.Command) bool {
	parent := cmd.Parent()
	if parent == nil || parent.Name() != "operations" || parent.Parent() == nil || parent.Parent().Name() != "identity" {
		return false
	}
	switch identitycontrol.Operation(cmd.Name()) {
	case identitycontrol.OperationGraphLink, identitycontrol.OperationGraphUnlink, identitycontrol.OperationPersonLink, identitycontrol.OperationPersonUnlink:
		return true
	}
	return cmd.Name() == "receipt"
}
