package cmd

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/msgvault/internal/documentindex"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

const documentFingerprintFlag = "if-fingerprint"
const documentJSONWritten = "document-json-written"

type documentCommandError struct{ code string }

func (e *documentCommandError) Error() string { return e.code }

func addDocumentJSONFlags(command *cobra.Command) {
	command.Flags().Bool(flagJSON, false, "Output one structured result, including partial failures")
	command.Flags().String(documentFingerprintFlag, "", "Require the exact current processing policy fingerprint before any mutation")
}
func documentJSONOutput(command *cobra.Command) bool {
	enabled, _ := command.Flags().GetBool(flagJSON)
	return enabled
}
func writeDocumentJSON(command *cobra.Command, output any) error {
	if err := json.MarshalWrite(command.OutOrStdout(), output); err != nil {
		return fmt.Errorf("write document result: %w", err)
	}
	if command.Annotations == nil {
		command.Annotations = map[string]string{}
	}
	command.Annotations[documentJSONWritten] = "true"
	return nil
}
func executeDocumentJSON(command *cobra.Command, execute func() error) error {
	if documentJSONOutput(command) {
		command.SilenceUsage = true
		command.SilenceErrors = true
	}
	err := execute()
	if err == nil || !documentJSONOutput(command) {
		return err
	}
	code := "document_execution_failed"
	uncertain := true
	if refusal, ok := errors.AsType[*documentCommandError](err); ok {
		code = refusal.code
		uncertain = false
	}
	if command.Annotations[documentJSONWritten] != "true" {
		if outputErr := writeDocumentJSON(command, struct {
			Error                     string `json:"error"`
			OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
		}{code, uncertain}); outputErr != nil {
			return errors.Join(err, outputErr)
		}
	}
	// Provider/parser diagnostics belong to host logs, not the structured stream.
	return &documentCommandError{code: code}
}
func exactDocumentFingerprint(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}
func guardDocumentFingerprint(command *cobra.Command, profile store.DocumentExtractionProfile) error {
	expected, _ := command.Flags().GetString(documentFingerprintFlag)
	if expected == "" && !documentJSONOutput(command) {
		return nil
	}
	if !exactDocumentFingerprint(expected) {
		return &documentCommandError{code: "invalid_document_fingerprint"}
	}
	if expected != profile.Fingerprint {
		return &documentCommandError{code: "document_policy_changed"}
	}
	return nil
}
func cloneDocumentConfig(original *documentindex.DocumentsConfig) *documentindex.DocumentsConfig {
	snapshot := *original
	snapshot.Scope.MessageTypes = slices.Clone(original.Scope.MessageTypes)
	if original.Index.Lexical != nil {
		snapshot.Index.Lexical = new(*original.Index.Lexical)
	}
	if original.Index.StoreChunkText != nil {
		snapshot.Index.StoreChunkText = new(*original.Index.StoreChunkText)
	}
	return &snapshot
}
func newDocumentPolicyCmd(deps documentsCommandDeps) *cobra.Command {
	var capabilityPath string
	command := &cobra.Command{Use: "policy", Short: "Preview the authenticated document upload policy and exact consent", Args: cobra.NoArgs, RunE: func(command *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobraWithLocalFiles(command, args, nil)
		}
		return executeDocumentJSON(command, func() error { return runDocumentPolicy(command, capabilityPath, deps) })
	}}
	command.Flags().StringVar(&capabilityPath, "capabilities", "", "Authenticated Mistral capability manifest")
	command.Flags().Bool(flagJSON, false, "Output the current structured disclosure without mutation")
	_ = command.MarkFlagRequired("capabilities")
	return command
}
func runDocumentPolicy(command *cobra.Command, capabilityPath string, deps documentsCommandDeps) error {
	documentsConfig, _, inputPolicy, profile, err := configuredDocumentProfile(capabilityPath, invocationFromCommand(command))
	if err != nil {
		return err
	}
	st, cleanup, err := deps.openStore(command.Context())
	if err != nil {
		return err
	}
	defer cleanup()
	status, err := st.GetDocumentIndexStatus(command.Context(), profile.ID)
	if err != nil {
		return err
	}
	consentedAt, err := st.GetDocumentProviderConsentTime(command.Context(), profile.ID, profile.Fingerprint)
	if err != nil {
		return err
	}
	var disclosure bytes.Buffer
	printDocumentConsentDisclosure(&disclosure, documentsConfig, profile, inputPolicy)
	var policy mcpserver.DocumentPolicyDetails
	if err := json.Unmarshal(profile.PolicyJSON, &policy, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decode canonical document policy: %w", err)
	}
	result := mcpserver.DocumentPolicy{ProfileID: profile.ID, Fingerprint: profile.Fingerprint, Enabled: documentsConfig.Enabled, ProfileExists: status.ProfileExists, ProfileEnabled: status.ProfileEnabled, Region: profile.Region, Policy: policy, Disclosure: disclosure.String(), ExactConsent: status.ExactConsent, ConsentedAt: consentedAt}
	if documentJSONOutput(command) {
		return writeDocumentJSON(command, result)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "%sFingerprint: %s\nProfile enabled: %t\nExact recorded consent: %t\n", disclosure.String(), profile.Fingerprint, status.ProfileEnabled, status.ExactConsent)
	if err != nil {
		return fmt.Errorf("write document policy disclosure: %w", err)
	}
	return nil
}
func documentBuildJSONResult(command *cobra.Command, st *store.Store, profile store.DocumentExtractionProfile, config *documentindex.DocumentsConfig, mediaTypes []string, result documentBuildResult, runErr error) error {
	coverage, coverageErr := st.GetDocumentIndexStatusForScope(command.Context(), profile.ID, "original", mediaTypes, config.Scope.MessageTypes)
	// Read declared additive diagnostics from the owning status DTO. Embedding
	// alone would hide them behind this bridge's optional compatibility fields.
	data, encodeErr := json.Marshal(coverage)
	if encodeErr != nil {
		return errors.Join(runErr, coverageErr, encodeErr)
	}
	var publicCoverage mcpserver.DocumentCoverage
	if err := json.Unmarshal(data, &publicCoverage, json.RejectUnknownMembers(true)); err != nil {
		return errors.Join(runErr, coverageErr, err)
	}
	if !validMCPDocumentFailures(publicCoverage.Failures) {
		return errors.Join(runErr, coverageErr, errors.New("invalid declared document failure diagnostics"))
	}
	output := mcpserver.DocumentBuild{ProfileID: profile.ID, Fingerprint: profile.Fingerprint, RunID: result.RunID, State: "completed", Attempted: result.Processed + result.Failed, Succeeded: result.Processed, Failed: result.Failed, Skipped: result.Skipped, Units: result.Units, Reconciled: result.Reconciled, Changes: result.Changes, CleanupFailures: result.CleanupFailures, RebuildID: result.RebuildID, Remaining: result.Remaining, Completed: result.Completed, Failures: []mcpserver.DocumentFailure{}, FailuresExhausted: len(result.Failures) <= 20, Coverage: publicCoverage}
	if runErr != nil || coverageErr != nil {
		output.State = string(startupCacheBuildOutcomeFailed)
	}
	if result.RebuildID != "" && !result.Completed && runErr == nil {
		output.State = "pending"
	}
	for _, failure := range result.Failures[:min(len(result.Failures), 20)] {
		// The optional owning producer adds Detail. Decode that declared field
		// without copying its implementation into the common bridge.
		data, err := json.Marshal(failure)
		if err != nil {
			return errors.Join(runErr, err)
		}
		var diagnostic struct{ Detail string }
		if err := json.Unmarshal(data, &diagnostic); err != nil {
			return errors.Join(runErr, err)
		}
		output.Failures = append(output.Failures, mcpserver.DocumentFailure{CanonicalBlobHash: failure.CanonicalBlobHash, ReasonCode: failure.ReasonCode, Detail: diagnostic.Detail})
	}
	return errors.Join(runErr, coverageErr, writeDocumentJSON(command, output))
}
