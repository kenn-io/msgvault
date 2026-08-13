package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/documentindex/mistral"
	internalmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestProbeMistralCommandWritesCompleteSanitizedManifest(t *testing.T) {
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut

	processor := &commandProbeProcessor{}
	loaderCalled := false
	cleanupCalled := false
	deps := documentsCommandDeps{
		newMistralProcessor: func(got *documentindex.DocumentsConfig, mediaTypes []string) (mistral.Processor, error) {
			assert.Same(&cfg.Attachments.Documents, got)
			assert.Len(mediaTypes, len(mistral.CandidateFormats()))
			return processor, nil
		},
		loadProbeFixtures: func(_ context.Context, directory string, maxBytes int64) (map[string]mistral.Document, func() error, error) {
			loaderCalled = true
			assert.Equal("synthetic-fixtures", directory)
			assert.Equal(cfg.Attachments.Documents.MaxFileBytes, maxBytes)
			documents := make(map[string]mistral.Document, len(mistral.CandidateFormats()))
			for _, candidate := range mistral.CandidateFormats() {
				documents[candidate.ID] = mistral.Document{
					Path: candidate.ID, MediaType: candidate.MediaType, Size: 1, SHA256: strings.Repeat("0", 64),
				}
			}
			return documents, func() error { cleanupCalled = true; return nil }, nil
		},
	}
	command := newDocumentsCmd(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"probe-mistral", "--fixtures", "synthetic-fixtures"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.True(loaderCalled)
	assert.True(cleanupCalled)
	manifest, err := mistral.DecodeCapabilityManifest(bytes.NewReader(output.Bytes()))
	require.NoError(t, err)
	require.Len(t, manifest.Results, len(mistral.CandidateFormats()))
	assert.Equal(mistral.ProbeStatusPassed, manifest.Results[0].Status)
	assert.NotContains(output.String(), "synthetic-fixtures")
}

func TestProbeMistralValidateOnlyNeedsNoProviderConfiguration(t *testing.T) {
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	providerCalled := false
	cleanupCalled := false
	deps := documentsCommandDeps{
		newMistralProcessor: func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error) {
			providerCalled = true
			return &commandProbeProcessor{}, nil
		},
		loadProbeFixtures: func(_ context.Context, directory string, maxBytes int64) (map[string]mistral.Document, func() error, error) {
			assert.Equal("synthetic-fixtures", directory)
			assert.Equal(cfg.Attachments.Documents.MaxFileBytes, maxBytes)
			documents := make(map[string]mistral.Document, len(mistral.CandidateFormats()))
			for _, candidate := range mistral.CandidateFormats() {
				documents[candidate.ID] = mistral.Document{MediaType: candidate.MediaType}
			}
			return documents, func() error { cleanupCalled = true; return nil }, nil
		},
	}
	command := newDocumentsCmd(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"probe-mistral", "--fixtures", "synthetic-fixtures", "--validate-only"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.False(providerCalled)
	assert.True(cleanupCalled)
	assert.Contains(output.String(), "Validated 26 private Mistral fixture(s) locally")
	assert.Contains(output.String(), "no provider requests")
	assert.NotContains(output.String(), "synthetic-fixtures")
}

func TestDocumentsConsentBuildAndStatusUseExactAuthenticatedProfile(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	content := []byte("%PDF-1.7\nsynthetic document")
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
		newMistralProcessor: func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error) {
			return processor, nil
		},
		loadProbeFixtures: mistral.LoadProbeFixtures,
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
	assert.Equal(len(mistral.CandidateFormats()), structuredStatus.AuthenticatedFormats)
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

func TestDocumentsSearchUsesConfiguredReadClient(t *testing.T) {
	openStoreCalled := false
	cleanupCalled := false
	reader := commandDocumentReadClient{
		search: func(
			_ context.Context,
			request store.DocumentSearchRequest,
		) (store.DocumentSearchResponse, error) {
			assert.Equal(t, "damage report", request.Query)
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
	command.SetArgs([]string{"search", "damage report", "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.False(t, openStoreCalled)
	assert.True(t, cleanupCalled)
	assert.Contains(t, output.String(), "report.pdf")
}

func TestDocumentsStatusUsesConfiguredReadClient(t *testing.T) {
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
			assert.NotEmpty(t, request.ProfileID)
			assert.Equal(t, "original", request.ExtractionInputKey)
			assert.NotEmpty(t, request.AllowedMediaTypes)
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
	assert.False(t, openStoreCalled)
	assert.True(t, cleanupCalled)
	assert.Contains(t, output.String(), `"ready_owners":3`)
}

func TestDocumentsBuildRefusesAPIUseBeforeExactConsent(t *testing.T) {
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
		newMistralProcessor: func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error) {
			providerCalled = true
			return &commandProbeProcessor{}, nil
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
		[]byte("%PDF-1.7\nfirst rebuild document"),
		[]byte("%PDF-1.7\nsecond rebuild document"),
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
		newMistralProcessor: func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error) {
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
	profile, err := configuredDocumentProfileOnly(manifestPath)
	require.NoError(err)
	_, err = fixture.Store.GetActiveDocumentExtractionRebuild(t.Context(), profile.ID, "original")
	require.ErrorIs(err, store.ErrDocumentExtractionRebuildMissing)
}

func TestDocumentBuildContinuesAfterOneExtractionFailure(t *testing.T) {
	require := require.New(t)
	fixture := storetest.New(t)
	contents := make(map[string][]byte)
	for index, content := range [][]byte{
		[]byte("%PDF-1.7\nmalformed synthetic document"),
		[]byte("%PDF-1.7\nsearchable synthetic document"),
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
		t.Context(), fixture.Store, commandAttachmentMapOpener{contents: contents},
		&commandFailFirstProcessor{}, &documentsConfig, manifest, allowed, profile, 2,
		"documents-isolation-test", t.TempDir(), documentBuildIncremental, nil,
	)
	require.ErrorContains(err, "1 extraction failure")
	assert.Equal(t, 1, result.Processed)
	assert.Equal(t, 1, result.Failed)
	response, searchErr := fixture.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "Synthetic"})
	require.NoError(searchErr)
	require.Len(response.Results, 1, "the second document must publish after the first is rejected")
}

func TestDocumentBuildStopsOnCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	content := []byte("%PDF-1.7\nsynthetic canceled document")
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

	result, err := executeDocumentBuild(
		t.Context(), fixture.Store, commandAttachmentMapOpener{contents: map[string][]byte{digest: content}},
		commandCancelingProcessor{}, &documentsConfig, manifest, allowed, profile, 1,
		"documents-cancellation-test", t.TempDir(), documentBuildIncremental, nil,
	)
	require.ErrorIs(err, context.Canceled)
	assert.Zero(result.Failed)
	assert.Zero(result.Processed)
	status, err := fixture.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	require.NoError(err)
	assert.Zero(status.StagingOwners, "provider cancellation must release the claim")
	assert.Equal(int64(1), status.RetryOwners)
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

func TestScheduledDocumentReconcileBootstrapsExistingConsent(t *testing.T) {
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
	require.NoError(configureDocumentReconcileJob(t.Context(), sched, fixture.Store, true))
	assert.Equal(1, commandDocumentOccurrenceCount(t, fixture.Store))
}

func TestScheduledDocumentExtractionUsesExactManifestConsentAndRunBudget(t *testing.T) {
	require := require.New(t)
	fixture := storetest.New(t)
	content := []byte("%PDF-1.7\nscheduled synthetic document")
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	messageID := fixture.CreateMessage("documents-scheduled-build")
	require.NoError(fixture.Store.UpsertMessageRaw(messageID, []byte(
		"From: sender@example.com\r\nTo: recipient@example.com\r\n"+
			"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=scheduled\r\n\r\n"+
			"--scheduled\r\nContent-Type: text/plain\r\n\r\nbody\r\n"+
			"--scheduled\r\nContent-Type: application/pdf\r\n"+
			"Content-Disposition: attachment; filename=scheduled.pdf\r\n\r\n"+
			string(content)+"\r\n--scheduled--\r\n")))
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "scheduled.pdf", MIMEType: "application/pdf", Size: int64(len(content)),
		StoragePath: digest[:2] + "/" + digest, ContentHash: digest,
	}))

	documentsConfig := documentindex.DefaultDocumentsConfig()
	documentsConfig.Enabled = true
	documentsConfig.RetentionPosture = documentindex.RetentionStandard
	documentsConfig.TrainingPosture = documentindex.TrainingOptedOut
	documentsConfig.Schedule = "17 * * * *"
	documentsConfig.EstimatedCostUSDPerKUnits = 4
	documentsConfig.PricingAssumptionOn = "2026-08-13"
	documentsConfig.CapabilityManifest = writeCommandCapabilityManifest(
		t, documentsConfig.MaxPagesPerDocument,
	)
	require.NoError(documentsConfig.Validate())
	manifest, err := loadDocumentCapabilityManifest(documentsConfig.CapabilityManifest)
	require.NoError(err)
	_, profile, err := documentProfileForConfig(&documentsConfig, manifest)
	require.NoError(err)
	_, err = fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	processor := &commandBuildProcessor{}
	sched := scheduler.New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(configureDocumentExtractionJob(sched, fixture.Store, documentsConfig, scheduledDocumentDeps{
		newProcessor: func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error) {
			return processor, nil
		},
		openAttachments: func(*store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
			return commandAttachmentOpener{content: content}, func() error { return nil }, nil
		},
		dataDirectory: t.TempDir(),
	}))
	assert.True(t, sched.IsJobScheduled(documentExtractionJob))
	require.NoError(sched.TriggerJob(documentExtractionJob))
	assert.Equal(t, 1, processor.calls)
	response, err := fixture.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "Synthetic"})
	require.NoError(err)
	require.Len(response.Results, 1)
	assert.Equal(t, messageID, response.Results[0].MessageID)
}

func TestProbeMistralCommandRequiresExplicitEnablementAndPosture(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	previousConfig := cfg
	t.Cleanup(func() { cfg = previousConfig })
	cfg = config.NewDefaultConfig()
	providerCalled := false
	deps := documentsCommandDeps{
		newMistralProcessor: func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error) {
			providerCalled = true
			return &commandProbeProcessor{}, nil
		},
		loadProbeFixtures: mistral.LoadProbeFixtures,
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

type commandProbeProcessor struct{}

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

func (p *commandProbeProcessor) Target() mistral.ProcessorTarget {
	return mistral.ProcessorTarget{
		Endpoint: "https://api.mistral.ai/v1/ocr",
		Region:   documentindex.RegionMistralEU,
		Model:    documentindex.ModelMistralOCR,
	}
}

type commandBuildProcessor struct {
	calls int
}

type commandFailFirstProcessor struct {
	calls int
}

type commandCancelingProcessor struct{}

func (commandCancelingProcessor) Target() mistral.ProcessorTarget {
	return (&commandProbeProcessor{}).Target()
}

func (commandCancelingProcessor) Process(
	context.Context,
	mistral.Document,
	mistral.Options,
) (mistral.Result, error) {
	return mistral.Result{}, context.Canceled
}

func (p *commandFailFirstProcessor) Target() mistral.ProcessorTarget {
	return (&commandProbeProcessor{}).Target()
}

func (p *commandFailFirstProcessor) Process(
	context.Context,
	mistral.Document,
	mistral.Options,
) (mistral.Result, error) {
	p.calls++
	if p.calls == 1 {
		return mistral.Result{}, mistral.ErrPermanentResponse
	}
	return mistral.Result{
		Model:     documentindex.ModelMistralOCR,
		Pages:     []mistral.Page{{Index: 0, Markdown: "# Indexed\nSynthetic evidence"}},
		UsageInfo: &mistral.Usage{PagesProcessed: 1},
	}, nil
}

func (p *commandBuildProcessor) Target() mistral.ProcessorTarget {
	return (&commandProbeProcessor{}).Target()
}

func (p *commandBuildProcessor) Process(
	context.Context,
	mistral.Document,
	mistral.Options,
) (mistral.Result, error) {
	p.calls++
	text := "# Indexed\nSynthetic evidence"
	if p.calls > 1 {
		text = "# Indexed\nReplacement evidence"
	}
	return mistral.Result{
		Model:     documentindex.ModelMistralOCR,
		Pages:     []mistral.Page{{Index: 0, Markdown: text}},
		UsageInfo: &mistral.Usage{PagesProcessed: 1},
	}, nil
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
	documents := make(map[string]mistral.Document, len(mistral.CandidateFormats()))
	for _, candidate := range mistral.CandidateFormats() {
		documents[candidate.ID] = mistral.Document{
			Path: candidate.ID, MediaType: candidate.MediaType, Size: 1, SHA256: strings.Repeat("0", 64),
		}
	}
	manifest, err := mistral.RunCapabilityProbe(t.Context(), &commandProbeProcessor{}, documents, mistral.ProbeConfig{
		ObservedAt: time.Now().UTC(), MaxPages: maxPages,
	})
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, mistral.EncodeCapabilityManifest(&encoded, manifest))
	path := filepath.Join(t.TempDir(), "capabilities.json")
	require.NoError(t, os.WriteFile(path, encoded.Bytes(), 0o600))
	return path
}

func commandDocumentOccurrenceCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM document_occurrences`).Scan(&count))
	return count
}

func (p *commandProbeProcessor) Process(
	_ context.Context,
	document mistral.Document,
	_ mistral.Options,
) (mistral.Result, error) {
	candidate, ok := mistral.CandidateFormatByMediaType(document.MediaType)
	if !ok {
		return mistral.Result{}, errors.New("synthetic processor received unknown media type")
	}
	sentinel, err := mistral.ProbeFixtureSentinel(candidate.ID)
	if err != nil {
		return mistral.Result{}, err
	}
	return mistral.Result{
		Model: documentindex.ModelMistralOCR,
		Pages: []mistral.Page{{Index: 0, Markdown: sentinel}},
		UsageInfo: &mistral.Usage{
			PagesProcessed: 1,
		},
	}, nil
}
