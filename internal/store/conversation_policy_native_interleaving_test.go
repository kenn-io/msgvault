//go:build pgvector && sqlite_vec

package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/visual"
)

// The extra SQL handle is only a native-table blocker/lock observer. Both
// participating Stores and the native backend's borrowed pool remain Max1.
func TestPostgresColdPolicyNativeLanesSerializePublicationAndTransition(t *testing.T) {
	for _, lane := range []string{"person", "document", "visual"} {
		t.Run(lane, func(t *testing.T) {
			f, documentGen := seedDocumentVectorGenerationWithChunks(t, 1)
			st := f.Store
			if !st.IsPostgreSQL() {
				t.Skip("requires actual PostgreSQL with pgvector")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			st.DB().SetMaxOpenConns(1)
			peer, err := store.OpenForTest(store.DBPathForTest(st))
			require.NoError(t, err)
			defer func() { require.NoError(t, peer.Close()) }()
			peer.DB().SetMaxOpenConns(1)
			observer, err := sql.Open("pgx", store.DBPathForTest(st))
			require.NoError(t, err)
			defer func() { require.NoError(t, observer.Close()) }()
			observer.SetMaxOpenConns(1)
			backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), PolicyStore: st, Dimension: 768})
			require.NoError(t, err)
			sourceID, roomKey := f.Source.ID, "default-thread"
			require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, roomKey, conversationpolicy.Full))
			var table string
			var publish func(context.Context) error
			var prune func(context.Context) (int64, error)
			var checkDemoted func()
			switch lane {
			case "person":
				source, err := st.GetOrCreateSource("slack", "native-person-interleave")
				require.NoError(t, err)
				room, err := st.EnsureConversation(source.ID, "person-room", "Synthetic person room")
				require.NoError(t, err)
				sourceID, roomKey = source.ID, "person-room"
				require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, roomKey, conversationpolicy.Full))
				participant, err := st.EnsureParticipantByIdentifier("slack", "native-person", "Native Person")
				require.NoError(t, err)
				person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
				require.NoError(t, err)
				_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
				require.NoError(t, err)
				message, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: room, SourceMessageID: "room-only-fact", MessageType: "chat", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true}})
				require.NoError(t, err)
				require.NoError(t, st.UpsertMessageBodyContext(ctx, message, sql.NullString{String: "Ask me about distributed systems.", Valid: true}, sql.NullString{}))
				inputs, err := st.HydratePersonSweepMessages(ctx, person.ID, []int64{message}, 0)
				require.NoError(t, err)
				require.Len(t, inputs, 1)
				evidence, err := peoplesweep.PersonFactEvidenceInput(inputs[0])
				require.NoError(t, err)
				catalog, err := st.BuildPersonFactCatalogContext(ctx, true)
				require.NoError(t, err)
				var target personfacts.TargetDescriptor
				for _, candidate := range catalog.Targets {
					if candidate.Slug == store.AttributeSlugAskMeAbout {
						target = candidate
					}
				}
				require.NotEmpty(t, target.Key)
				result, err := st.ApplyPersonFactGenerationContext(ctx, personfacts.GenerationInput{
					PersonID: person.ID, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "native-interleave", End: "native-interleave-end"}},
					ProgramID: "native-interleave", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "catalog-fixture",
					Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: time.Now().UTC(),
					Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"},
					Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport, SubmittedValue: []byte(`"distributed systems"`), Evidence: []personfacts.EvidenceInput{evidence}, Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}}},
				}, store.PersonSweepEvidenceAligner{Store: st})
				require.NoError(t, err)
				require.NotEmpty(t, result.Projections)
				semantic, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
				require.NoError(t, err)
				require.Contains(t, semantic.Text, "distributed systems")
				gen, err := backend.CreateGeneration(ctx, "native-person-interleave", 768, "")
				require.NoError(t, err)
				embedding := make([]float32, 768)
				embedding[0] = 1
				publish = func(work context.Context) error {
					return backend.UpsertPersons(work, gen, []vector.PersonEmbedding{{PersonID: person.ID, Revision: semantic.Revision, Vector: embedding}})
				}
				prune = backend.PruneIneligiblePersons
				table = "person_embeddings"
				checkDemoted = func() {
					current, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
					require.NoError(t, err)
					assert.NotContains(t, current.Text, "distributed systems")
					assert.NotEqual(t, semantic.Revision, current.Revision)
				}
			case "document":
				claim, err := st.ClaimDocumentVectorChunk(ctx, documentGen.ID, 0, 10, "native-interleave", time.Now().UTC(), time.Minute)
				require.NoError(t, err)
				require.NotNil(t, claim)
				embedding := make([]float32, 768)
				embedding[0] = 1
				documents := backend.DocumentBackend()
				publish = func(work context.Context) error {
					return documents.PutUnpublished(work, document.GenerationID(documentGen.ID), 768, []document.Embedding{{Token: claim.Token, Vector: embedding}})
				}
				prune = documents.PruneIneligibleDocuments
				table = "document_vector_embeddings"
			case "visual":
				gen := createVisualGeneration(t, f)
				owner, attachment, fence := createVisualOwner(t, f, "native-visual-interleave")
				claim := claimVisualOwner(t, f, gen.ID, owner, "revision", fence)
				token, err := st.PrepareVisualPublication(ctx, store.PreparedVisualPublication{Claim: claim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
				require.NoError(t, err)
				embedding := make([]float32, 1024)
				embedding[0] = 1
				visuals := backend.Visual()
				publish = func(work context.Context) error {
					return visuals.PutUnpublished(work, visual.VectorToken(token), embedding)
				}
				prune = visuals.PruneIneligibleVisuals
				table = "visual_vectors"
			}
			work, err := st.CaptureConversationPolicyWork(ctx)
			require.NoError(t, err)
			require.NoError(t, publish(work), "successful current native publication control")
			var count int
			require.NoError(t, st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
			require.Equal(t, 1, count)
			// Clear only this synthetic native row so cleanup below proves the paused
			// publication inserted a row, rather than merely deleting the control.
			_, err = st.DB().ExecContext(ctx, "DELETE FROM "+table)
			require.NoError(t, err)
			var publicationPID, transitionPID, observerPID int
			require.NoError(t, st.DB().QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&publicationPID))
			require.NoError(t, peer.DB().QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&transitionPID))
			blocker, err := observer.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback() }()
			require.NoError(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&observerPID))
			_, err = blocker.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE MODE")
			require.NoError(t, err)
			published, transitioned := make(chan error, 1), make(chan error, 1)
			go func() { published <- publish(work) }()
			waitNativePolicyLock(ctx, t, blocker, published, "native write waits on instrumentation", `SELECT EXISTS(
   SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='relation' AND relation=$2::regclass
   AND mode='RowExclusiveLock' AND NOT granted) AND $3=ANY(pg_blocking_pids($1))`, publicationPID, table, observerPID)
			go func() {
				transitioned <- peer.SetConversationPolicyOverride(ctx, sourceID, roomKey, conversationpolicy.Cold)
			}()
			waitNativePolicyLock(ctx, t, blocker, transitioned, "demotion waits on actual publication policy R", `SELECT EXISTS(
   SELECT 1 FROM pg_locks waiting JOIN pg_locks held
    ON held.locktype=waiting.locktype AND held.database=waiting.database
    AND held.classid=waiting.classid AND held.objid=waiting.objid AND held.objsubid=waiting.objsubid
   WHERE waiting.pid=$1 AND held.pid=$2 AND waiting.locktype='advisory'
    AND waiting.mode='ExclusiveLock' AND NOT waiting.granted
    AND held.mode='ShareLock' AND held.granted) AND $2=ANY(pg_blocking_pids($1))`, transitionPID, publicationPID)
			require.NoError(t, blocker.Commit())
			require.NoError(t, awaitNativePolicyOperation(ctx, t, published))
			require.NoError(t, awaitNativePolicyOperation(ctx, t, transitioned))
			if checkDemoted != nil {
				checkDemoted()
			}
			removed, err := prune(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(1), removed)
			require.NoError(t, st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
			assert.Zero(t, count)
			assert.ErrorIs(t, publish(work), store.ErrConversationPolicyStale, "original work cannot repollute after demotion and cleanup")
		})
	}
}

func waitNativePolicyLock(ctx context.Context, t *testing.T, observer *sql.Tx, operation <-chan error, label, query string, args ...any) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var found bool
		require.NoError(t, observer.QueryRowContext(ctx, query, args...).Scan(&found), label)
		if found {
			return
		}
		select {
		case err := <-operation:
			require.FailNowf(t, "operation completed before required lock boundary", "%s: %v", label, err)
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), label)
		case <-ticker.C:
		}
	}
}

func awaitNativePolicyOperation(ctx context.Context, t *testing.T, operation <-chan error) error {
	t.Helper()
	select {
	case err := <-operation:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
