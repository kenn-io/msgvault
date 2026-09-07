package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	internalmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

func TestProbeMistralCommandWritesCompleteSanitizedManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut

	probeCalled := false
	deps := documentsCommandDeps{
		newMistralClient: func(got *documentindex.DocumentsConfig) (*mistral.Client, error) {
			assert.Same(&cfg.Attachments.Documents, got)
			return new(mistral.Client), nil
		},
		runCapabilityProbe: func(_ context.Context, _ *mistral.Client, got mistral.ProbeConfig) (mistral.CapabilityManifest, error) {
			probeCalled = true
			assert.Equal("synthetic-fixtures", got.Fixtures.FixtureDirectory)
			return commandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument), nil
		},
	}
	command := newDocumentsCmd(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"probe-mistral", "--fixtures", "synthetic-fixtures"})

	require.NoError(command.ExecuteContext(t.Context()))
	assert.True(probeCalled)
	manifest, err := mistral.DecodeCapabilityManifest(bytes.NewReader(output.Bytes()))
	require.NoError(err)
	require.Len(manifest.Results, len(mistral.CandidateFormats()))
	assert.Equal(mistral.ProbeStatusPassed, manifest.Results[0].Status)
	assert.NotContains(output.String(), "synthetic-fixtures")
}

func TestProbeMistralValidateOnlyNeedsNoProviderConfiguration(t *testing.T) {
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	providerCalled := false
	validationCalled := false
	deps := documentsCommandDeps{
		newMistralClient: func(*documentindex.DocumentsConfig) (*mistral.Client, error) {
			providerCalled = true
			return nil, errors.New("unexpected provider client construction")
		},
		validateProbeFixtures: func(_ context.Context, _ mistral.Policy, got mistral.ProbeFixtureConfig) error {
			validationCalled = true
			assert.Equal("synthetic-fixtures", got.FixtureDirectory)
			return nil
		},
	}
	command := newDocumentsCmd(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"probe-mistral", "--fixtures", "synthetic-fixtures", "--validate-only"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.False(providerCalled)
	assert.True(validationCalled)
	assert.Contains(output.String(), "Validated 26 private Mistral fixture(s) locally")
	assert.Contains(output.String(), "no provider requests")
	assert.NotContains(output.String(), "synthetic-fixtures")
}

func TestDocumentsConsentBuildAndStatusUseExactAuthenticatedProfile(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	markDaemonCLISubprocessForTest(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	cfg.Attachments.Documents.EstimatedCostUSDPerKUnits = 4
	cfg.Attachments.Documents.PricingAssumptionOn = "2026-08-13"

	fixture := storetest.New(t)
	content := mistraltest.MinimalPDF("synthetic document")
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	messageID := fixture.CreateMessage("documents-command")
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)),
		StoragePath: digest[:2] + "/" + digest, ContentHash: digest,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	manifestPath := writeCommandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument)
	processor := &commandBuildProcessor{}
	attachmentOpened := false
	deps := documentsCommandDeps{
		newMistralProcessor: func(*documentindex.DocumentsConfig) (documentindex.MistralProcessor, error) {
			return processor, nil
		},
		openStore: func() (*store.Store, func(), error) {
			return fixture.Store, func() {}, nil
		},
		openAttachments: func(*store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
			attachmentOpened = true
			return commandAttachmentOpener{content: content}, func() error { return nil }, nil
		},
	}

	unconfirmedConsent := newDocumentsCmd(deps)
	var disclosureOutput bytes.Buffer
	unconfirmedConsent.SetOut(&disclosureOutput)
	unconfirmedConsent.SetErr(&bytes.Buffer{})
	unconfirmedConsent.SetArgs([]string{"consent-mistral", "--capabilities", manifestPath})
	require.ErrorContains(unconfirmedConsent.ExecuteContext(t.Context()), "requires --yes")
	assert.Contains(disclosureOutput.String(), "Complete original document bytes")
	assert.Contains(disclosureOutput.String(), "retention=standard, training=opted-out")
	assert.Contains(disclosureOutput.String(), "local archive database")
	assert.Contains(disclosureOutput.String(), "does not enable hosted document text embeddings")
	assert.NotContains(disclosureOutput.String(), manifestPath)

	consent := newDocumentsCmd(deps)
	var consentOutput bytes.Buffer
	consent.SetOut(&consentOutput)
	consent.SetErr(&bytes.Buffer{})
	consent.SetArgs([]string{"consent-mistral", "--capabilities", manifestPath, "--yes"})
	require.NoError(consent.ExecuteContext(t.Context()))
	assert.Contains(consentOutput.String(), "Recorded Mistral document consent")
	assert.NotContains(consentOutput.String(), manifestPath)
	assert.Equal(1, commandDocumentOccurrenceCount(t, fixture.Store),
		"consent must bootstrap historical attachment occurrences before the first build")

	build := newDocumentsCmd(deps)
	var buildOutput bytes.Buffer
	build.SetOut(&buildOutput)
	build.SetErr(&bytes.Buffer{})
	build.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--limit", "5"})
	require.ErrorContains(build.ExecuteContext(t.Context()), "requires --yes")
	assert.Contains(buildOutput.String(), "Document build upload preflight")
	assert.Contains(buildOutput.String(), "Complete original document bytes")
	assert.False(attachmentOpened)
	assert.Zero(processor.calls)

	build = newDocumentsCmd(deps)
	buildOutput.Reset()
	build.SetOut(&buildOutput)
	build.SetErr(&bytes.Buffer{})
	build.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--limit", "5", "--yes"})
	require.NoError(build.ExecuteContext(t.Context()))
	assert.Contains(buildOutput.String(), "indexed 1 document(s), 1 unit(s), skipped 0, failed 0")
	assert.Equal(1, processor.calls)

	search := newDocumentsCmd(deps)
	var searchOutput bytes.Buffer
	search.SetOut(&searchOutput)
	search.SetErr(&bytes.Buffer{})
	search.SetArgs([]string{"search", "Synthetic", "--json"})
	require.NoError(search.ExecuteContext(t.Context()))
	var searchResponse store.DocumentSearchResponse
	require.NoError(json.Unmarshal(searchOutput.Bytes(), &searchResponse))
	require.Len(searchResponse.Results, 1)
	assert.Equal(messageID, searchResponse.Results[0].MessageID)
	assert.Equal("synthetic.pdf", searchResponse.Results[0].Filename)
	assert.Contains(searchResponse.Results[0].MatchedSignals, "content")

	resume := newDocumentsCmd(deps)
	var resumeOutput bytes.Buffer
	resume.SetOut(&resumeOutput)
	resume.SetErr(&bytes.Buffer{})
	resume.SetArgs([]string{"resume", "--capabilities", manifestPath, "--limit", "5", "--yes"})
	require.NoError(resume.ExecuteContext(t.Context()))
	assert.Contains(resumeOutput.String(), "indexed 0 document(s)")
	assert.Equal(1, processor.calls)

	rebuild := newDocumentsCmd(deps)
	rebuild.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--full-rebuild"})
	require.ErrorContains(rebuild.ExecuteContext(t.Context()), "requires --yes")
	assert.Equal(1, processor.calls)
	rebuild = newDocumentsCmd(deps)
	var rebuildOutput bytes.Buffer
	rebuild.SetOut(&rebuildOutput)
	rebuild.SetErr(&bytes.Buffer{})
	rebuild.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--full-rebuild", "--yes"})
	require.NoError(rebuild.ExecuteContext(t.Context()))
	assert.Contains(rebuildOutput.String(), "indexed 1 document(s)")
	assert.Equal(2, processor.calls)
	search = newDocumentsCmd(deps)
	searchOutput.Reset()
	search.SetOut(&searchOutput)
	search.SetErr(&bytes.Buffer{})
	search.SetArgs([]string{"search", "Replacement", "--json"})
	require.NoError(search.ExecuteContext(t.Context()))
	require.NoError(json.Unmarshal(searchOutput.Bytes(), &searchResponse))
	require.Len(searchResponse.Results, 1)
	assert.Equal(messageID, searchResponse.Results[0].MessageID)

	status := newDocumentsCmd(deps)
	var statusOutput bytes.Buffer
	status.SetOut(&statusOutput)
	status.SetErr(&bytes.Buffer{})
	status.SetArgs([]string{"status", "--capabilities", manifestPath})
	require.NoError(status.ExecuteContext(t.Context()))
	assert.Contains(statusOutput.String(), "Exact consent: true")
	assert.Contains(statusOutput.String(), "Coverage: 1 ready")
	assert.Contains(statusOutput.String(), "Extraction accounting: 2 attempt(s), 2 successful, 0 failed")
	assert.Contains(statusOutput.String(), "Provider accounting: 2 request(s), 0 internal retry(s)")
	assert.Contains(statusOutput.String(), "Normalized plaintext stored: true")
	assert.Contains(statusOutput.String(), "Hosted document text embeddings: false")

	statusJSON := newDocumentsCmd(deps)
	var statusJSONOutput bytes.Buffer
	statusJSON.SetOut(&statusJSONOutput)
	statusJSON.SetErr(&bytes.Buffer{})
	statusJSON.SetArgs([]string{"status", "--capabilities", manifestPath, "--json"})
	require.NoError(statusJSON.ExecuteContext(t.Context()))
	var structuredStatus documentStatusOutput
	require.NoError(json.Unmarshal(statusJSONOutput.Bytes(), &structuredStatus))
	assert.Equal(1, structuredStatus.AuthenticatedFormats)
	assert.True(structuredStatus.Status.ExactConsent)
	assert.Equal(int64(1), structuredStatus.Status.ReadyOwners)
	assert.Equal(int64(1), structuredStatus.Status.EligibleOwners)
	assert.Equal(int64(len(content)), structuredStatus.Status.EligibleBytes)
	assert.Equal(int64(2), structuredStatus.Status.ExtractionAttempts)
	assert.Equal(int64(2), structuredStatus.Status.SuccessfulAttempts)
	assert.Equal(int64(2), structuredStatus.Status.ProviderRequests)
	assert.Zero(structuredStatus.Status.ProviderRetries)
	assert.Positive(structuredStatus.Status.ProviderLatencyMillis)
	assert.Positive(structuredStatus.Status.AverageProviderLatencyMS)
	assert.Equal(int64(2*len(content)), structuredStatus.Status.VerifiedUploadBytes)
	assert.Equal(int64(2), structuredStatus.Status.ProcessedProviderUnits)
	assert.Equal(int64(2), structuredStatus.Status.MissingProviderByteReports)
	assert.Equal(documentindex.ModelMistralOCR, structuredStatus.Model)
	assert.Equal(documentindex.RetentionStandard, structuredStatus.RetentionPosture)
	assert.True(structuredStatus.StoresPlaintext)
	assert.True(structuredStatus.BackupsMayContainText)
	assert.False(structuredStatus.HostedTextEmbeddings)
	assert.Equal("2026-08-13", structuredStatus.PricingAssumptionOn)
	require.NotNil(structuredStatus.EstimatedSuccessfulCostUSD)
	assert.InDelta(0.008, *structuredStatus.EstimatedSuccessfulCostUSD, 0.000001)

	profile, err := configuredDocumentProfileOnly(manifestPath)
	require.NoError(err)
	retry := newDocumentsCmd(deps)
	retry.SetArgs([]string{"retry", "--capabilities", manifestPath, "--hash", digest})
	require.ErrorContains(retry.ExecuteContext(t.Context()), "already current")

	retire := newDocumentsCmd(deps)
	retire.SetArgs([]string{"retire", profile.ID})
	require.ErrorContains(retire.ExecuteContext(t.Context()), "requires --yes")
	retire = newDocumentsCmd(deps)
	var retireOutput bytes.Buffer
	retire.SetOut(&retireOutput)
	retire.SetErr(&bytes.Buffer{})
	retire.SetArgs([]string{"retire", profile.ID, "--yes"})
	require.NoError(retire.ExecuteContext(t.Context()))
	assert.Contains(retireOutput.String(), "Retired document extraction profile")

	search = newDocumentsCmd(deps)
	searchOutput.Reset()
	search.SetOut(&searchOutput)
	search.SetErr(&bytes.Buffer{})
	search.SetArgs([]string{"search", "Synthetic", "--json"})
	require.NoError(search.ExecuteContext(t.Context()))
	require.NoError(json.Unmarshal(searchOutput.Bytes(), &searchResponse))
	assert.Empty(searchResponse.Results)

	purge := newDocumentsCmd(deps)
	purge.SetArgs([]string{"purge-derived", "--hash", digest})
	require.ErrorContains(purge.ExecuteContext(t.Context()), "requires --yes")
	purge = newDocumentsCmd(deps)
	var purgeOutput bytes.Buffer
	purge.SetOut(&purgeOutput)
	purge.SetErr(&bytes.Buffer{})
	purge.SetArgs([]string{"purge-derived", "--hash", digest, "--yes"})
	require.NoError(purge.ExecuteContext(t.Context()))
	assert.Contains(purgeOutput.String(), "Purged 2 extraction(s) and 1 current head(s)")
}

func TestDocumentBuildRepairsHistoricalMIMERolesBeforePreflight(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	require := require.New(t)
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut

	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("documents-historical-role")
	raw := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=doc\r\n\r\n" +
		"--doc\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--doc\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=archive.pdf\r\n\r\n%PDF-synthetic\r\n" +
		"--doc--\r\n")
	require.NoError(fixture.Store.UpsertMessageRaw(messageID, raw))
	parsed, err := internalmime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 1)
	attachment := parsed.Attachments[0]
	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		messageID, attachment.Filename, attachment.ContentType,
		attachment.ContentHash[:2]+"/"+attachment.ContentHash,
		attachment.ContentHash, len(attachment.Content))
	require.NoError(err)

	manifestPath := writeCommandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument)
	deps := documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		openAttachments: func(*store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
			return nil, func() error { return nil }, errors.New("synthetic stop after repair")
		},
	}
	command := newDocumentsCmd(deps)
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath})
	require.ErrorContains(command.ExecuteContext(t.Context()), "requires --yes")

	var role, roleSource string
	var partKey *string
	require.NoError(fixture.Store.DB().QueryRow(fixture.Store.Rebind(`
		SELECT attachment_role, role_source, source_part_key
		FROM attachments WHERE message_id = ?`), messageID).Scan(&role, &roleSource, &partKey))
	assert.Equal("unknown", role, "declining build must not mutate historical attachment roles")
	assert.Equal("unknown", roleSource)
	assert.Nil(partKey)

	consent := newDocumentsCmd(deps)
	consent.SetOut(&bytes.Buffer{})
	consent.SetErr(&bytes.Buffer{})
	consent.SetArgs([]string{"consent-mistral", "--capabilities", manifestPath, "--yes"})
	require.NoError(consent.ExecuteContext(t.Context()))
	command = newDocumentsCmd(deps)
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--yes"})
	require.ErrorContains(command.ExecuteContext(t.Context()), "synthetic stop after repair")

	require.NoError(fixture.Store.DB().QueryRow(fixture.Store.Rebind(`
		SELECT attachment_role, role_source, source_part_key
		FROM attachments WHERE message_id = ?`), messageID).Scan(&role, &roleSource, &partKey))
	assert.Equal("standalone", role)
	assert.Equal("raw_mime_repair", roleSource)
	require.NotNil(partKey)
	assert.NotEmpty(*partKey)
}

func TestDocumentsSearchDoesNotRegisterUnconsentedJournalConsumer(t *testing.T) {
	require := require.New(t)
	fixture := storetest.New(t)
	deps := documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
	}
	command := newDocumentsCmd(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"search", "nothing", "--json"})
	require.NoError(command.ExecuteContext(t.Context()))
	var response store.DocumentSearchResponse
	require.NoError(json.Unmarshal(output.Bytes(), &response))
	assert.Empty(t, response.Results)
	_, err := fixture.Store.GetAttachmentChangeConsumer(
		t.Context(), documentindex.DocumentAttachmentConsumerKey,
	)
	require.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
}

func TestDocumentsSearchLocalExplicitSemanticNeverMasqueradesAsLexical(t *testing.T) {
	fixture := storetest.New(t)
	command := newDocumentsCmd(documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
	})
	command.SetArgs([]string{"search", "evidence", "--mode", "semantic", "--candidate-limit", "25"})
	err := command.ExecuteContext(t.Context())
	require.ErrorIs(t, err, vectordocument.ErrSemanticSearchUnavailable)
}

func TestDocumentsSearchUsesConfiguredReadClient(t *testing.T) {
	assert := assert.New(t)
	openStoreCalled := false
	cleanupCalled := false
	reader := commandDocumentReadClient{
		search: func(
			_ context.Context,
			request store.DocumentSearchRequest,
		) (store.DocumentSearchResponse, error) {
			assert.Equal("damage report", request.Query)
			assert.Zero(request.CandidateLimit)
			assert.Equal(int64(40), request.PersonID)
			assert.Equal([]personscope.Direction{personscope.FromPerson, personscope.Group}, request.Directions)
			require.NotNil(t, request.After)
			require.NotNil(t, request.Before)
			return store.DocumentSearchResponse{Results: []store.DocumentSearchResult{{
				AttachmentID: 3, MessageID: 4, Filename: "report.pdf", Rank: 1,
			}}}, nil
		},
	}
	command := newDocumentsCmd(documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) {
			openStoreCalled = true
			return nil, func() {}, errors.New("local store must not be opened")
		},
		openReadClient: func(context.Context) (documentReadClient, func(), error) {
			return reader, func() { cleanupCalled = true }, nil
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"search", "damage report", "--person", "40",
		"--direction", "from_person,group", "--after", "2026-08-01", "--before", "2026-08-20", "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.False(openStoreCalled)
	assert.True(cleanupCalled)
	assert.Contains(output.String(), "report.pdf")
}

func TestDocumentsSearchRejectsNonPositivePersonScopeBeforeDispatch(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		value string
	}{
		{name: "zero person", flag: "--person", value: "0"},
		{name: "negative person", flag: "--person", value: "-1"},
		{name: "zero participant", flag: "--participant", value: "0"},
		{name: "negative participant", flag: "--participant", value: "-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dispatched := false
			command := newDocumentsCmd(documentsCommandDeps{
				openReadClient: func(context.Context) (documentReadClient, func(), error) {
					dispatched = true
					return nil, func() {}, errors.New("unexpected read client dispatch")
				},
			})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs([]string{"search", "damage report", test.flag, test.value})

			err := command.ExecuteContext(t.Context())

			require.Error(t, err)
			require.ErrorContains(t, err, "must be a positive")
			assert.False(t, dispatched)
		})
	}
}

func TestDocumentsStatusUsesConfiguredReadClient(t *testing.T) {
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	manifestPath := writeCommandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument)
	openStoreCalled := false
	cleanupCalled := false
	reader := commandDocumentReadClient{
		status: func(
			_ context.Context,
			request store.DocumentIndexStatusRequest,
		) (store.DocumentIndexStatusResponse, error) {
			assert.NotEmpty(request.ProfileID)
			assert.Equal("original", request.ExtractionInputKey)
			assert.NotEmpty(request.AllowedMediaTypes)
			return store.DocumentIndexStatusResponse{Status: store.DocumentIndexStatus{
				ProfileExists: true, ReadyOwners: 3,
			}}, nil
		},
	}
	command := newDocumentsCmd(documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) {
			openStoreCalled = true
			return nil, func() {}, errors.New("local store must not be opened")
		},
		openReadClient: func(context.Context) (documentReadClient, func(), error) {
			return reader, func() { cleanupCalled = true }, nil
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"status", "--capabilities", manifestPath, "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.False(openStoreCalled)
	assert.True(cleanupCalled)
	assert.Contains(output.String(), `"ready_owners":3`)
}

func TestDocumentsBuildRefusesAPIUseBeforeExactConsent(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	fixture := storetest.New(t)
	providerCalled := false
	deps := documentsCommandDeps{
		newMistralProcessor: func(*documentindex.DocumentsConfig) (documentindex.MistralProcessor, error) {
			providerCalled = true
			return &commandBuildProcessor{}, nil
		},
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
	}
	manifestPath := writeCommandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument)
	command := newDocumentsCmd(deps)
	command.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--yes"})
	err := command.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "requires exact consent")
	assert.False(t, providerCalled)
}

func TestDocumentFullRebuildResumesDurableTargetSnapshot(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	require := require.New(t)
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	fixture := storetest.New(t)
	contents := make(map[string][]byte)
	for index, content := range [][]byte{
		mistraltest.MinimalPDF("first rebuild document"),
		mistraltest.MinimalPDF("second rebuild document"),
	} {
		digestBytes := sha256.Sum256(content)
		digest := hex.EncodeToString(digestBytes[:])
		contents[digest] = content
		messageID := fixture.CreateMessage("documents-rebuild-" + string(rune('a'+index)))
		require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)),
			StoragePath: digest[:2] + "/" + digest, ContentHash: digest,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
			SourcePartKey: "part:" + string(rune('1'+index)),
		}))
	}
	manifestPath := writeCommandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument)
	processor := &commandBuildProcessor{}
	deps := documentsCommandDeps{
		newMistralProcessor: func(*documentindex.DocumentsConfig) (documentindex.MistralProcessor, error) {
			return processor, nil
		},
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		openAttachments: func(*store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
			return commandAttachmentMapOpener{contents: contents}, func() error { return nil }, nil
		},
	}
	consent := newDocumentsCmd(deps)
	consent.SetArgs([]string{"consent-mistral", "--capabilities", manifestPath, "--yes"})
	require.NoError(consent.ExecuteContext(t.Context()))
	initial := newDocumentsCmd(deps)
	initial.SetArgs([]string{documentBuildSubcommand, "--capabilities", manifestPath, "--limit", "2", "--yes"})
	require.NoError(initial.ExecuteContext(t.Context()))
	assert.Equal(2, processor.calls)

	rebuild := newDocumentsCmd(deps)
	var rebuildOutput bytes.Buffer
	rebuild.SetOut(&rebuildOutput)
	rebuild.SetErr(&bytes.Buffer{})
	rebuild.SetArgs([]string{
		documentBuildSubcommand, "--capabilities", manifestPath,
		"--full-rebuild", "--yes", "--limit", "1",
	})
	require.NoError(rebuild.ExecuteContext(t.Context()))
	assert.Contains(rebuildOutput.String(), "1 current owner(s) remaining")
	assert.Equal(3, processor.calls)
	profile, err := configuredDocumentProfileOnly(manifestPath)
	require.NoError(err)
	privateRebuild, err := fixture.Store.GetActiveDocumentExtractionRebuild(
		t.Context(), profile.ID, "original",
	)
	require.NoError(err)
	status := newDocumentsCmd(deps)
	var statusOutput bytes.Buffer
	status.SetOut(&statusOutput)
	status.SetErr(&bytes.Buffer{})
	status.SetArgs([]string{"status", "--capabilities", manifestPath, "--json"})
	require.NoError(status.ExecuteContext(t.Context()))
	var structuredStatus documentStatusOutput
	require.NoError(json.Unmarshal(statusOutput.Bytes(), &structuredStatus))
	require.NotNil(structuredStatus.ActiveRebuild)
	assert.Equal(int64(2), structuredStatus.ActiveRebuild.SnapshotOwners)
	assert.Equal(int64(1), structuredStatus.ActiveRebuild.RemainingOwners)

	resume := newDocumentsCmd(deps)
	var resumeOutput bytes.Buffer
	resume.SetOut(&resumeOutput)
	resume.SetErr(&bytes.Buffer{})
	resume.SetArgs([]string{"resume", "--capabilities", manifestPath, "--limit", "1", "--yes"})
	require.NoError(resume.ExecuteContext(t.Context()))
	assert.Contains(resumeOutput.String(), "Full document rebuild completed")
	assert.Equal(4, processor.calls)
	_, err = fixture.Store.GetActiveDocumentExtractionRebuild(t.Context(), profile.ID, "original")
	require.ErrorIs(err, store.ErrDocumentExtractionRebuildMissing)
	runs := operationRunsForKind(t, fixture.Store, operations.KindDocumentExtraction)
	require.Len(runs, 3, "incremental, rebuild, and resume are separate bounded passes")
	assert.NotContains(fmt.Sprint(runs), privateRebuild.ID,
		"the private rebuild reference must not enter public operation rows")
}

func TestDocumentBuildRecordsOversizedCandidateAndContinues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	contents := make(map[string][]byte)
	searchable := mistraltest.MinimalPDF("searchable synthetic document")
	oversized := append(mistraltest.MinimalPDF("oversized synthetic document"), bytes.Repeat([]byte("% padding\n"), 20)...)
	for index, content := range [][]byte{
		oversized,
		searchable,
	} {
		digestBytes := sha256.Sum256(content)
		digest := hex.EncodeToString(digestBytes[:])
		contents[digest] = content
		messageID := fixture.CreateMessage("documents-isolated-" + string(rune('a'+index)))
		require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)),
			StoragePath: digest[:2] + "/" + digest, ContentHash: digest,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
			SourcePartKey: "part:" + string(rune('1'+index)),
		}))
	}
	documentsConfig := documentindex.DefaultDocumentsConfig()
	documentsConfig.Enabled = true
	documentsConfig.RetentionPosture = documentindex.RetentionStandard
	documentsConfig.TrainingPosture = documentindex.TrainingOptedOut
	documentsConfig.MaxFileBytes = int64(len(searchable))
	manifestPath := writeCommandCapabilityManifest(t, documentsConfig.MaxPagesPerDocument)
	manifest, err := loadDocumentCapabilityManifest(manifestPath)
	require.NoError(err)
	allowed, profile, err := documentProfileForConfig(&documentsConfig, manifest)
	require.NoError(err)
	_, err = fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	result, err := executeDocumentBuild(
		t.Context(), fixture.Store, testOperationPassScope("document:oversized"),
		fixture.Store, commandAttachmentMapOpener{contents: contents},
		&commandBuildProcessor{}, &documentsConfig, manifest, allowed, profile, 2,
		"documents-isolation-test", t.TempDir(), documentBuildIncremental, nil,
	)
	require.ErrorContains(err, "1 extraction failure")
	assert.Equal(1, result.Processed)
	assert.Equal(1, result.Failed)
	runs := operationRunsForKind(t, fixture.Store, operations.KindDocumentExtraction)
	require.Len(runs, 1)
	assert.Equal(operations.StatePartial, runs[0].State)
	assert.Equal([]operations.PublicCounter{
		{Name: operations.CounterAttempted, Unit: operations.CounterUnitDocuments, Value: 2},
		{Name: operations.CounterSucceeded, Unit: operations.CounterUnitDocuments, Value: 1},
		{Name: operations.CounterFailed, Unit: operations.CounterUnitDocuments, Value: 1},
	}, runs[0].Counters)
	response, searchErr := fixture.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "Synthetic"})
	require.NoError(searchErr)
	require.Len(response.Results, 1, "the valid document must publish after the oversized candidate is recorded")
}

func TestDocumentBuildRequiresRecorderBeforeWork(t *testing.T) {
	result, err := executeDocumentBuild(
		t.Context(), nil, testOperationPassScope("document:missing-recorder"),
		nil, nil, nil, nil, mistral.CapabilityManifest{}, nil, store.DocumentExtractionProfile{}, 1,
		"documents-recorder-test", t.TempDir(), documentBuildIncremental, nil,
	)

	require.ErrorContains(t, err, "operation recorder is required")
	assert.Equal(t, documentBuildResult{}, result)
}

func TestDocumentBuildTerminalReplayReturnsOnlyFixedNonSuccess(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 30, 15, 30, 0, 0, time.UTC)
	finished := started.Add(time.Second)
	trigger := operations.TriggerManual
	id, err := operations.NewInt64ID(operations.KindDocumentExtraction, 9)
	require.NoError(t, err)
	tests := []struct {
		name      string
		state     operations.State
		counters  operations.InvocationCounters
		publicErr *operations.PublicError
		wantIs    error
		wantErr   bool
	}{
		{name: "success", state: operations.StateSucceeded,
			counters: operations.InvocationCounters{Attempted: 1, Succeeded: 1}},
		{name: "partial", state: operations.StatePartial,
			counters:  operations.InvocationCounters{Attempted: 2, Succeeded: 1, Failed: 1},
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed)},
		{name: "failed", state: operations.StateFailed,
			counters:  operations.InvocationCounters{Attempted: 1, Failed: 1},
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed), wantErr: true},
		{name: "cancelled", state: operations.StateCancelled,
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationCancelled),
			wantIs:    context.Canceled, wantErr: true},
		{name: "timed out", state: operations.StateFailed,
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationTimeout),
			wantIs:    context.DeadlineExceeded, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			run := &operations.Run{
				ID: id, Lane: operations.LaneDocuments, State: test.state, Trigger: &trigger,
				StartedAt: started, FinishedAt: &finished,
				Counters: test.counters.PublicCounters(operations.KindDocumentExtraction), Error: test.publicErr,
			}
			result, replayErr := documentBuildResultFromOperationRun(run)
			assert.Equal(int(test.counters.Succeeded), result.Processed)
			assert.Equal(int(test.counters.Failed), result.Failed)
			if !test.wantErr {
				require.NoError(replayErr)
				return
			}
			require.Error(replayErr)
			assert.NotContains(replayErr.Error(), "private provider response")
			if test.wantIs != nil {
				assert.ErrorIs(replayErr, test.wantIs)
			}
		})
	}
}

func TestDocumentBuildStopsOnCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	content := mistraltest.MinimalPDF("synthetic canceled document")
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	messageID := fixture.CreateMessage("documents-canceled")
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)),
		StoragePath: digest[:2] + "/" + digest, ContentHash: digest,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	documentsConfig := documentindex.DefaultDocumentsConfig()
	documentsConfig.Enabled = true
	documentsConfig.RetentionPosture = documentindex.RetentionStandard
	documentsConfig.TrainingPosture = documentindex.TrainingOptedOut
	manifestPath := writeCommandCapabilityManifest(t, documentsConfig.MaxPagesPerDocument)
	manifest, err := loadDocumentCapabilityManifest(manifestPath)
	require.NoError(err)
	allowed, profile, err := documentProfileForConfig(&documentsConfig, manifest)
	require.NoError(err)
	_, err = fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	ctx, cancel := context.WithCancel(t.Context())
	result, err := executeDocumentBuild(
		ctx, fixture.Store, testOperationPassScope("document:cancelled"),
		fixture.Store, commandAttachmentMapOpener{contents: map[string][]byte{digest: content}},
		commandCancelingProcessor{cancel: cancel}, &documentsConfig, manifest, allowed, profile, 1,
		"documents-cancellation-test", t.TempDir(), documentBuildIncremental, nil,
	)
	require.ErrorIs(err, context.Canceled)
	assert.Zero(result.Failed)
	assert.Zero(result.Processed)
	status, err := fixture.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	require.NoError(err)
	assert.Zero(status.StagingOwners, "provider cancellation must release the claim")
	assert.Equal(int64(1), status.RetryOwners)
	runs := operationRunsForKind(t, fixture.Store, operations.KindDocumentExtraction)
	require.Len(runs, 1, "detached finish must survive caller cancellation")
	assert.Equal(operations.StateCancelled, runs[0].State)
}

func TestDocumentBuildContinuesAfterProviderTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	contents := make(map[string][]byte)
	var failedDigest string
	for index, text := range []string{"timed out document", "searchable document"} {
		content := mistraltest.MinimalPDF(text)
		digestBytes := sha256.Sum256(content)
		digest := hex.EncodeToString(digestBytes[:])
		contents[digest] = content
		if index == 0 {
			failedDigest = digest
		}
		messageID := fixture.CreateMessage("documents-timeout-" + string(rune('a'+index)))
		require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)),
			StoragePath: digest[:2] + "/" + digest, ContentHash: digest,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
			SourcePartKey: "part:" + string(rune('1'+index)),
		}))
	}
	documentsConfig := documentindex.DefaultDocumentsConfig()
	documentsConfig.Enabled = true
	documentsConfig.RetentionPosture = documentindex.RetentionStandard
	documentsConfig.TrainingPosture = documentindex.TrainingOptedOut
	manifestPath := writeCommandCapabilityManifest(t, documentsConfig.MaxPagesPerDocument)
	manifest, err := loadDocumentCapabilityManifest(manifestPath)
	require.NoError(err)
	allowed, profile, err := documentProfileForConfig(&documentsConfig, manifest)
	require.NoError(err)
	_, err = fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	result, err := executeDocumentBuild(
		t.Context(), fixture.Store, testOperationPassScope("document:timeout"),
		fixture.Store, commandAttachmentMapOpener{contents: contents},
		&commandBuildProcessor{firstErr: context.DeadlineExceeded},
		&documentsConfig, manifest, allowed, profile, 2,
		"documents-timeout-test", t.TempDir(), documentBuildIncremental, nil,
	)
	require.ErrorContains(err, "1 extraction failure")
	require.ErrorContains(err, failedDigest)
	require.ErrorContains(err, "provider_interrupted")
	assert.Equal(1, result.Processed)
	assert.Equal(1, result.Failed)
	assert.Equal([]documentBuildFailure{{
		CanonicalBlobHash: failedDigest,
		ReasonCode:        "provider_interrupted",
	}}, result.Failures)
	assert.Equal(2, result.Processed+result.Failed)
}

func TestScheduledDocumentFullReconcileRepairsMissedLifecycleEvent(t *testing.T) {
	require := require.New(t)
	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("documents-scheduled-reconcile")
	hash := strings.Repeat("e", 64)
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 64,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	reconciler, err := documentindex.NewReconciler(fixture.Store, documentindex.ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.Equal(t, 1, commandDocumentOccurrenceCount(t, fixture.Store))

	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, err = fixture.Store.DB().Exec(`DELETE FROM attachment_change_log`)
	require.NoError(err)

	sched := scheduler.New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(configureDocumentReconcileJob(t.Context(), sched, fixture.Store, true))
	assert.True(t, sched.IsJobScheduled(documentFullReconcileJob))
	require.NoError(sched.TriggerJob(documentFullReconcileJob))
	assert.Zero(t, commandDocumentOccurrenceCount(t, fixture.Store))
}

func TestLocalDocumentStatusReconcilesAttachmentJournalBeforeCounting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	reconciler, err := documentindex.NewReconciler(fixture.Store, documentindex.ReconcilerConfig{
		AttachmentPageSize: 10,
		ChangePageSize:     10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)

	messageID := fixture.CreateMessage("documents-status-freshness")
	hash := strings.Repeat("f", 64)
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "fresh.pdf", MIMEType: "application/pdf", Size: 64,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:fresh",
	}))
	assert.Zero(commandDocumentOccurrenceCount(t, fixture.Store))

	response, err := (localDocumentReadClient{store: fixture.Store}).GetDocumentIndexStatus(
		t.Context(), store.DocumentIndexStatusRequest{
			ProfileID: "missing-profile", ExtractionInputKey: "original",
			AllowedMediaTypes: []string{"application/pdf"},
		},
	)
	require.NoError(err)
	assert.Equal(int64(1), response.Status.EligibleOccurrences)
	assert.Equal(1, commandDocumentOccurrenceCount(t, fixture.Store))
}

func TestScheduledDocumentReconcileDoesNotEnableUnconsentedConsumer(t *testing.T) {
	fixture := storetest.New(t)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(t, configureDocumentReconcileJob(t.Context(), sched, fixture.Store, true))
	require.NoError(t, sched.TriggerJob(documentFullReconcileJob))
	_, err := fixture.Store.GetAttachmentChangeConsumer(
		t.Context(), documentindex.DocumentAttachmentConsumerKey,
	)
	require.ErrorIs(t, err, store.ErrAttachmentChangeConsumerMissing)
}

func TestScheduledDocumentReconcilePreservesExistingConsentWhenExtractionDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("documents-scheduler-bootstrap")
	hash := strings.Repeat("a", 64)
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "existing.pdf", MIMEType: "application/pdf", Size: 64,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:existing",
	}))
	fingerprint := strings.Repeat("b", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint,
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: documentindex.ModelMistralOCR,
		RetentionPosture:  string(documentindex.RetentionStandard),
		TrainingPosture:   string(documentindex.TrainingOptedOut),
		AllowedMediaTypes: []string{"application/pdf"},
		PolicyJSON:        []byte(`{"normalization":1}`),
	}
	_, err := fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	sched := scheduler.New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(configureDocumentReconcileJob(t.Context(), sched, fixture.Store, false))
	assert.True(sched.IsJobScheduled(documentFullReconcileJob))
	assert.Equal(1, commandDocumentOccurrenceCount(t, fixture.Store))
}

func TestProbeMistralCommandRequiresExplicitEnablementAndPosture(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	providerCalled := false
	deps := documentsCommandDeps{
		newMistralClient: func(*documentindex.DocumentsConfig) (*mistral.Client, error) {
			providerCalled = true
			return nil, errors.New("unexpected provider client construction")
		},
	}

	command := newDocumentsCmd(deps)
	command.SetArgs([]string{"probe-mistral", "--fixtures", "synthetic-fixtures"})
	err := command.ExecuteContext(t.Context())
	require.ErrorContains(err, "enabled=true")
	assert.False(providerCalled)

	cfg.Attachments.Documents.Enabled = true
	command = newDocumentsCmd(deps)
	command.SetArgs([]string{"probe-mistral", "--fixtures", "synthetic-fixtures"})
	err = command.ExecuteContext(t.Context())
	require.ErrorContains(err, "explicit retention_posture and training_posture")
	assert.False(providerCalled)
}

type commandDocumentReadClient struct {
	search func(context.Context, store.DocumentSearchRequest) (store.DocumentSearchResponse, error)
	status func(context.Context, store.DocumentIndexStatusRequest) (store.DocumentIndexStatusResponse, error)
}

func (c commandDocumentReadClient) SearchDocuments(
	ctx context.Context,
	request store.DocumentSearchRequest,
) (store.DocumentSearchResponse, error) {
	if c.search == nil {
		return store.DocumentSearchResponse{}, errors.New("unexpected document search")
	}
	return c.search(ctx, request)
}

func (c commandDocumentReadClient) GetDocumentIndexStatus(
	ctx context.Context,
	request store.DocumentIndexStatusRequest,
) (store.DocumentIndexStatusResponse, error) {
	if c.status == nil {
		return store.DocumentIndexStatusResponse{}, errors.New("unexpected document status read")
	}
	return c.status(ctx, request)
}

type commandBuildProcessor struct {
	calls    int
	firstErr error
}

type commandCancelingProcessor struct {
	cancel context.CancelFunc
}

func (p commandCancelingProcessor) Process(
	context.Context,
	*mistral.PreparedDocument,
	mistral.FormatAuthorization,
) (mistral.Result, error) {
	p.cancel()
	return mistral.Result{}, context.Canceled
}

func (p *commandBuildProcessor) Process(
	context.Context,
	*mistral.PreparedDocument,
	mistral.FormatAuthorization,
) (mistral.Result, error) {
	p.calls++
	if p.calls == 1 && p.firstErr != nil {
		return mistral.Result{}, p.firstErr
	}
	text := "# Indexed\nSynthetic evidence"
	if p.calls > 1 {
		text = "# Indexed\nReplacement evidence"
	}
	return commandMistralResult(text), nil
}

type commandAttachmentOpener struct {
	content []byte
}

type commandAttachmentMapOpener struct {
	contents map[string][]byte
}

func (o commandAttachmentMapOpener) OpenStream(_ context.Context, hash string) (io.ReadCloser, int64, error) {
	content, ok := o.contents[hash]
	if !ok {
		return nil, 0, errors.New("synthetic attachment was not found")
	}
	return io.NopCloser(bytes.NewReader(content)), int64(len(content)), nil
}

func (o commandAttachmentOpener) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(o.content)), int64(len(o.content)), nil
}

func writeCommandCapabilityManifest(t *testing.T, maxPages int) string {
	t.Helper()
	manifest := commandCapabilityManifest(t, maxPages)
	var encoded bytes.Buffer
	require.NoError(t, mistral.EncodeCapabilityManifest(&encoded, manifest))
	path := filepath.Join(t.TempDir(), "capabilities.json")
	require.NoError(t, os.WriteFile(path, encoded.Bytes(), 0o600))
	return path
}

func commandCapabilityManifest(t *testing.T, maxUnits int) mistral.CapabilityManifest {
	t.Helper()
	documentsConfig := documentindex.DefaultDocumentsConfig()
	documentsConfig.RetentionPosture = documentindex.RetentionZDR
	documentsConfig.TrainingPosture = documentindex.TrainingOptedOut
	documentsConfig.MaxPagesPerDocument = maxUnits
	policy, err := documentsConfig.MistralPolicy()
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	return manifest
}

func commandDocumentOccurrenceCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM document_occurrences`).Scan(&count))
	return count
}

func testOperationPassScope(key string) operations.PassScope {
	return operations.PassScope{
		Key: key, Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}
}

func operationRunsForKind(t *testing.T, st *store.Store, kind operations.Kind) []operations.Run {
	t.Helper()
	snapshot, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{kind}, Limit: 100,
	})
	require.NoError(t, err)
	return snapshot.Runs
}

func commandMistralResult(markdown string) mistral.Result {
	return mistral.Result{
		Document: document.SourceDocument{
			Family: "pdf", UnitKind: "page",
			Units: []document.SourceUnit{{Index: 0, Markdown: markdown}},
		},
		ReturnedModel: mistral.DefaultModel, UnitsProcessed: 1,
		Metrics: mistral.RequestMetrics{Requests: 1, Latency: time.Millisecond},
	}
}
