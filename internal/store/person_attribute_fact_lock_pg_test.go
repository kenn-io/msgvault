package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPostgreSQLManualPersonAttributeWritesLockBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, *Store, int64) error
	}{
		{
			name: "set",
			run: func(t *testing.T, st *Store, personID int64) error {
				t.Helper()
				_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
					PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
					Value:  AttributeValue{Type: AttributeValueText, Text: new("email")},
					Source: ProvenanceUser,
				})
				return err
			},
		},
		{
			name: "supersede",
			run: func(t *testing.T, st *Store, personID int64) error {
				t.Helper()
				seed, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
					PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
					Value:  AttributeValue{Type: AttributeValueText, Text: new("chat")},
					Source: ProvenanceExtraction,
				})
				if err != nil {
					return err
				}
				_, err = st.SupersedePersonAttributeValueContext(t.Context(), PersonAttributeSupersedeInput{
					PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
					ExpectedValueID: &seed.Value.ID,
				})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			if !st.IsPostgreSQL() {
				t.Skip("PostgreSQL advisory-lock concurrency regression")
			}
			target := targets[AttributeSlugPrimaryChannel]

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			t.Cleanup(cancel)
			blocker, err := st.db.BeginTx(ctx, nil)
			requirements.NoError(err)
			t.Cleanup(func() { _ = blocker.Rollback() })
			var blockerPID int
			requirements.NoError(blocker.QueryRowContext(ctx,
				`SELECT pg_backend_pid()`).Scan(&blockerPID))
			requirements.NoError(st.lockProfileIdentityKeyTxContext(ctx, blocker,
				"person-fact-target", personID, target.Kind, target.Key))

			writeDone := make(chan error, 1)
			go func() { writeDone <- test.run(t, st, personID) }()
			waitForManualPersonAttributeTargetLock(t, st, personID, blockerPID, writeDone)

			requirements.NoError(blocker.Commit())
			select {
			case writeErr := <-writeDone:
				requirements.NoError(writeErr)
			case <-ctx.Done():
				requirements.FailNow("manual person attribute write did not finish", ctx.Err())
			}

			pins, err := st.ListPersonFactPinsContext(ctx, personID)
			requirements.NoError(err)
			requirements.Len(pins, 1)
			assertions.Equal(personfacts.TargetAttribute, pins[0].Target.Kind)
			assertions.Equal(target.Key, pins[0].Target.Key)
			assertions.True(pins[0].Pinned)
		})
	}
}

func TestPostgreSQLSystemPersonAttributeWriteWaitsForFactResolution(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock concurrency regression")
	}
	target := targets[AttributeSlugPrimaryChannel]

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	blocker, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var blockerPID int
	requirements.NoError(blocker.QueryRowContext(ctx,
		`SELECT pg_backend_pid()`).Scan(&blockerPID))
	requirements.NoError(st.lockProfileIdentityKeyTxContext(ctx, blocker,
		"person-fact-target", personID, target.Kind, target.Key))

	generationInput := personFactProjectionInput(personID, "system-write-lock", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"email"`, "system-write-lock"),
	}, nil)
	type generationOutcome struct {
		result *personfacts.GenerationResult
		err    error
	}
	generationDone := make(chan generationOutcome, 1)
	go func() {
		result, applyErr := st.ApplyPersonFactGenerationContext(ctx, generationInput, nil)
		generationDone <- generationOutcome{result: result, err: applyErr}
	}()

	var generationPID int
	requirements.Eventually(func() bool {
		generationPID = personAttributePostgreSQLWaitingWriterPID(t, st, blockerPID)
		return generationPID > 0
	}, 5*time.Second, 10*time.Millisecond,
		"fact resolution did not wait for the blocked target lock")

	type attributeOutcome struct {
		write *PersonAttributeWrite
		err   error
	}
	attributeDone := make(chan attributeOutcome, 1)
	go func() {
		write, writeErr := st.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
			PersonID:       personID,
			DefinitionSlug: AttributeSlugPrimaryChannel,
			Value:          AttributeValue{Type: AttributeValueText, Text: new("phone")},
			Source:         ProvenanceSystem,
		})
		attributeDone <- attributeOutcome{write: write, err: writeErr}
	}()

	var earlyAttribute *attributeOutcome
	requirements.Eventually(func() bool {
		select {
		case outcome := <-attributeDone:
			earlyAttribute = &outcome
			return true
		default:
			return personAttributePostgreSQLBlockedWriterPID(t, st, generationPID) > 0
		}
	}, 5*time.Second, 10*time.Millisecond,
		"system attribute write neither completed nor waited for fact resolution")
	if earlyAttribute != nil {
		requirements.NoError(earlyAttribute.err)
		requirements.NotNil(earlyAttribute.write)
	}
	assertions.Nil(earlyAttribute,
		"system attribute write bypassed the in-flight fact resolution locks")

	requirements.NoError(blocker.Commit())
	select {
	case outcome := <-generationDone:
		requirements.NoError(outcome.err)
		requirements.NotNil(outcome.result)
	case <-ctx.Done():
		requirements.FailNow("fact resolution did not finish", ctx.Err())
	}
	if earlyAttribute == nil {
		select {
		case outcome := <-attributeDone:
			requirements.NoError(outcome.err)
			requirements.NotNil(outcome.write)
		case <-ctx.Done():
			requirements.FailNow("system attribute write did not finish", ctx.Err())
		}
	}
}

func waitForManualPersonAttributeTargetLock(
	t *testing.T, st *Store, personID int64, blockerPID int, writeDone <-chan error,
) {
	t.Helper()
	requirements := require.New(t)
	deadline := time.NewTimer(5 * time.Second)
	t.Cleanup(func() { deadline.Stop() })
	for {
		select {
		case err := <-writeDone:
			requirements.NoError(err)
			requirements.FailNow("manual person attribute write bypassed the target lock")
		case <-deadline.C:
			requirements.FailNow("manual person attribute write did not wait for the target lock")
		default:
		}

		probeCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		probe, err := st.db.BeginTx(probeCtx, nil)
		requirements.NoError(err)
		lockErr := st.lockProfileIdentityKeyTxContext(
			probeCtx, probe, "person-fact-generation", personID)
		_ = probe.Rollback()
		cancel()
		if errors.Is(lockErr, context.DeadlineExceeded) {
			requirements.Eventually(func() bool {
				return personAttributePostgreSQLWaitingWriterPID(t, st, blockerPID) > 0
			}, time.Second, 10*time.Millisecond,
				"manual write held the generation lock but did not wait for the target lock")
			return
		}
		requirements.NoError(lockErr)
	}
}

func personAttributePostgreSQLWaitingWriterPID(t *testing.T, st *Store, blockerPID int) int {
	t.Helper()
	var writerPID int
	require.NoError(t, st.db.QueryRowContext(t.Context(), `
		SELECT COALESCE(MIN(activity.pid), 0)
		FROM pg_stat_activity activity
		JOIN pg_locks waiting
		  ON waiting.pid = activity.pid
		 AND waiting.locktype = 'advisory'
		 AND NOT waiting.granted
		WHERE activity.datname = current_database()
		  AND activity.wait_event_type = 'Lock'
		  AND $1 = ANY(pg_blocking_pids(activity.pid))
		  AND EXISTS (
		      SELECT 1 FROM pg_locks held
		      WHERE held.pid = activity.pid
		        AND held.locktype = 'advisory'
		        AND held.granted
		  )`, blockerPID).Scan(&writerPID))
	return writerPID
}

func personAttributePostgreSQLBlockedWriterPID(t *testing.T, st *Store, blockerPID int) int {
	t.Helper()
	var writerPID int
	require.NoError(t, st.db.QueryRowContext(t.Context(), `
		SELECT COALESCE(MIN(activity.pid), 0)
		FROM pg_stat_activity activity
		JOIN pg_locks waiting
		  ON waiting.pid = activity.pid
		 AND waiting.locktype = 'advisory'
		 AND NOT waiting.granted
		WHERE activity.datname = current_database()
		  AND activity.wait_event_type = 'Lock'
		  AND $1 = ANY(pg_blocking_pids(activity.pid))`, blockerPID).Scan(&writerPID))
	return writerPID
}

func TestPostgreSQLDefinitionExposureSerializesBeforePeople(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("PostgreSQL catalog/person lock barrier")
	}
	for _, exposure := range []string{"activation", "seed mapping"} {
		for _, writer := range []string{"same definition note", "unrelated definition value"} {
			t.Run(exposure+"/"+writer, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				gate := newPersonFactEmploymentLockOrderGate(personFactEmploymentStagePerson)
				defer gate.release()
				st := newPersonFactEmploymentPostgresGateStore(t, dbURL, gate)
				first := inferencePerson(t, st, "catalog-writer@example.com")
				contributor := inferencePerson(t, st, "catalog-contributor@example.com")
				inferenceNote(t, st, contributor.ID, ProvenanceExtraction, "Seed")
				personID := contributor.ID
				if writer == "unrelated definition value" {
					personID = first.ID
				}
				definition, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugNotes)
				require.NoError(err)
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				writeCtx := context.WithValue(ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorDeclared)
				writeDone := make(chan error, 1)
				go func() {
					var err error
					if writer == "same definition note" {
						_, err = st.AppendPersonNoteContext(writeCtx, PersonNoteAppendInput{PersonID: personID, Text: "Curated", Source: ProvenanceUser})
					} else {
						_, err = st.SetPersonAttributeValueContext(writeCtx, PersonAttributeValueInput{PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel, Value: AttributeValue{Type: AttributeValueText, Text: new("email")}, Source: ProvenanceUser})
					}
					writeDone <- err
				}()
				waitPersonFactEmploymentLockSignal(t, gate.declaredPaused, "value writer did not reach person barrier")
				var writerPID int
				require.NoError(st.db.QueryRowContext(ctx, `SELECT activity.pid FROM pg_stat_activity activity
 WHERE activity.datname=current_database() AND activity.state='idle in transaction'
 AND EXISTS (SELECT 1 FROM pg_locks held WHERE held.pid=activity.pid
 AND held.relation='attribute_definitions'::regclass AND held.mode='RowShareLock' AND held.granted)`).Scan(&writerPID))
				exposureDone := make(chan error, 1)
				go func() {
					var err error
					if exposure == "activation" {
						_, err = st.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision, AttributeDefinitionUpdate{IsActive: new(false)})
					} else {
						var seed AttributeDefinitionInput
						for _, candidate := range SeededAttributeDefinitions() {
							if candidate.Slug == AttributeSlugNotes {
								seed = candidate
							}
						}
						seed.VCardProperty = nil
						err = st.reconcileSeededDefinition(ctx, definition, seed)
					}
					exposureDone <- err
				}()
				var early *error
				require.Eventually(func() bool {
					select {
					case err := <-exposureDone:
						early = &err
						return true
					default:
					}
					var blocked bool
					err := st.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity activity
 WHERE activity.datname=current_database() AND ?=ANY(pg_blocking_pids(activity.pid)))`, writerPID).Scan(&blocked)
					require.NoError(err)
					return blocked
				}, 5*time.Second, 10*time.Millisecond, "catalog exposure neither waited nor completed")
				assert.Nil(early, "global catalog exposure must wait for unrelated definition writers before locking people")
				gate.release()
				select {
				case err := <-writeDone:
					require.NoError(err)
				case <-ctx.Done():
					require.FailNow("value mutation did not finish", ctx.Err())
				}
				if early == nil {
					select {
					case err := <-exposureDone:
						require.NoError(err)
					case <-ctx.Done():
						require.FailNow("catalog exposure did not finish", ctx.Err())
					}
				} else {
					require.NoError(*early)
				}
				got, err := st.GetAttributeDefinitionBySlugContext(ctx, AttributeObjectPerson, AttributeSlugNotes)
				require.NoError(err)
				if exposure == "activation" {
					assert.False(got.IsActive)
				} else {
					assert.Nil(got.VCardProperty)
				}
			})
		}
	}
}
