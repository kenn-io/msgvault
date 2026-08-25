package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactEmploymentCrossedOrganizationChainsSerialize(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	postgres := IsPostgresURL(dbURL)
	var gate *personFactOrganizationCrossedLockGate
	var st *Store
	if postgres {
		gate = newPersonFactOrganizationCrossedLockGate(2)
		st = newPersonFactOrganizationCrossedLockStore(t, dbURL, gate)
	} else {
		st, _, _ = newPersonFactProjectionStore(t)
	}
	requirements := require.New(t)
	assertions := assert.New(t)

	firstPersonID := createTrackedPersonFactPerson(t, st, "crossed-org-first")
	secondPersonID := createTrackedPersonFactPerson(t, st, "crossed-org-second")
	target := projectionTargetBySlug(t, st, "employment")
	firstSource := createPersonFactOrganization(t, st, "First Source", "first-source.example")
	firstRoot := createPersonFactOrganization(t, st, "First Root", "first-root.example")
	secondSource := createPersonFactOrganization(t, st, "Second Source", "second-source.example")
	secondRoot := createPersonFactOrganization(t, st, "Second Root", "second-root.example")
	secondAlternate := createPersonFactOrganization(
		t, st, "Second Alternate", "second-alternate.example")
	firstAlternate := createPersonFactOrganization(
		t, st, "First Alternate", "first-alternate.example")
	mergedFirst, err := st.MergeOrganizationsContext(
		t.Context(), firstRoot.ID, firstRoot.Revision, firstSource.ID, firstSource.Revision)
	requirements.NoError(err)
	_, err = st.MergeOrganizationsContext(
		t.Context(), mergedFirst.ID, mergedFirst.Revision, firstAlternate.ID, firstAlternate.Revision)
	requirements.NoError(err)
	mergedSecond, err := st.MergeOrganizationsContext(
		t.Context(), secondRoot.ID, secondRoot.Revision, secondSource.ID, secondSource.Revision)
	requirements.NoError(err)
	_, err = st.MergeOrganizationsContext(
		t.Context(), mergedSecond.ID, mergedSecond.Revision,
		secondAlternate.ID, secondAlternate.Revision)
	requirements.NoError(err)

	firstInput := crossedPersonFactOrganizationGeneration(
		t, firstPersonID, target, firstSource, secondSource, firstSource.ID, "first")
	secondInput := crossedPersonFactOrganizationGeneration(
		t, secondPersonID, target, firstAlternate, secondAlternate,
		secondAlternate.ID, "second")
	if !postgres {
		first, firstErr := st.ApplyPersonFactGenerationContext(t.Context(), firstInput, nil)
		second, secondErr := st.ApplyPersonFactGenerationContext(t.Context(), secondInput, nil)
		requirements.NoError(firstErr)
		requirements.NoError(secondErr)
		requirements.Len(first.Projections, 2)
		requirements.Len(second.Projections, 2)
	} else {
		applyPersonFactCrossedOrganizationGenerations(t, st, gate, firstInput, secondInput)
	}
	assertions.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assertions.Equal(int64(4), personFactProjectionRowCount(t, st, "employments"))

	firstEvidence, err := st.ListPersonFactEvidenceContext(t.Context(), firstPersonID,
		personfacts.EvidenceFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(firstEvidence, 2)
	secondEvidence, err := st.ListPersonFactEvidenceContext(t.Context(), secondPersonID,
		personfacts.EvidenceFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(secondEvidence, 2)
	firstReplay := personFactProjectionInput(firstPersonID, "crossed-first-replay", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: firstEvidence[0].Key, SourceVersion: firstEvidence[0].Input.SourceVersion,
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	secondReplay := personFactProjectionInput(secondPersonID, "crossed-second-replay", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: secondEvidence[0].Key, SourceVersion: secondEvidence[0].Input.SourceVersion,
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	if !postgres {
		_, firstErr := st.ApplyPersonFactGenerationContext(t.Context(), firstReplay, nil)
		_, secondErr := st.ApplyPersonFactGenerationContext(t.Context(), secondReplay, nil)
		requirements.NoError(firstErr)
		requirements.NoError(secondErr)
	} else {
		gate.reset()
		applyPersonFactCrossedOrganizationGenerations(t, st, gate, firstReplay, secondReplay)
	}
	assertions.Equal(int64(4), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestPersonFactEmploymentIncomingAndHistoricalOrganizationsShareLockOrder(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	postgres := IsPostgresURL(dbURL)
	var gate *personFactOrganizationCrossedLockGate
	var st *Store
	if postgres {
		gate = newPersonFactOrganizationCrossedLockGate(1)
		st = newPersonFactOrganizationCrossedLockStore(t, dbURL, gate)
	} else {
		st, _, _ = newPersonFactProjectionStore(t)
	}
	requirements := require.New(t)
	assertions := assert.New(t)

	firstPersonID := createTrackedPersonFactPerson(t, st, "mixed-org-first")
	secondPersonID := createTrackedPersonFactPerson(t, st, "mixed-org-second")
	target := projectionTargetBySlug(t, st, "employment")
	firstOrganization := createPersonFactOrganization(
		t, st, "Mixed First Organization", "mixed-first.example")
	secondOrganization := createPersonFactOrganization(
		t, st, "Mixed Second Organization", "mixed-second.example")
	claim := func(personID int64, organization *Organization, title, suffix string) personfacts.ProposedClaim {
		return personFactProjectionClaim(personID, target, fmt.Sprintf(
			`{"organization":{"id":%d,"name":%q,"domain":%q},"title":%q}`,
			organization.ID, organization.Name, *organization.PrimaryDomain, title), suffix)
	}
	_, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(firstPersonID, "mixed-first-history", []personfacts.ProposedClaim{
			claim(firstPersonID, secondOrganization, "Historical Engineer", "mixed-first-history"),
		}, nil), nil)
	requirements.NoError(err)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(secondPersonID, "mixed-second-history", []personfacts.ProposedClaim{
			claim(secondPersonID, firstOrganization, "Historical Engineer", "mixed-second-history"),
		}, nil), nil)
	requirements.NoError(err)

	firstIncoming := personFactProjectionInput(
		firstPersonID, "mixed-first-incoming", []personfacts.ProposedClaim{
			claim(firstPersonID, firstOrganization, "Incoming Engineer", "mixed-first-incoming"),
		}, nil)
	secondIncoming := personFactProjectionInput(
		secondPersonID, "mixed-second-incoming", []personfacts.ProposedClaim{
			claim(secondPersonID, secondOrganization, "Incoming Engineer", "mixed-second-incoming"),
		}, nil)
	if postgres {
		applyPersonFactCrossedOrganizationGenerations(
			t, st, gate, firstIncoming, secondIncoming)
	} else {
		_, firstErr := st.ApplyPersonFactGenerationContext(t.Context(), firstIncoming, nil)
		_, secondErr := st.ApplyPersonFactGenerationContext(t.Context(), secondIncoming, nil)
		requirements.NoError(firstErr)
		requirements.NoError(secondErr)
	}
	assertions.Equal(int64(4), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assertions.Equal(int64(4), personFactProjectionRowCount(t, st, "employments"))
}

func TestPersonFactEmploymentNonIDCandidateSnapshotPrecedesOrganizationMutation(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("PostgreSQL table locks are required for the candidate snapshot race")
	}
	for _, mode := range []string{"rename", "retire", "newly-matching"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			gate := newPersonFactOrganizationCandidateGate(2)
			st := newPersonFactOrganizationCandidateGateStore(t, dbURL, gate)
			personID := createTrackedPersonFactPerson(t, st, "candidate-race-"+mode)
			target := projectionTargetBySlug(t, st, "employment")
			candidate := createPersonFactOrganization(
				t, st, "Candidate Race Company", "candidate-race.example")
			var contender *Organization
			if mode == "newly-matching" {
				contender = createPersonFactOrganization(
					t, st, "Candidate Race Contender", "candidate-race-contender.example")
			}
			claim := personFactProjectionClaim(personID, target,
				`{"organization":{"name":"Candidate Race Company","domain":"candidate-race.example"},"title":"Engineer"}`,
				"candidate-race-"+mode)
			input := personFactProjectionInput(personID, "candidate-race-"+mode,
				[]personfacts.ProposedClaim{claim}, nil)

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			t.Cleanup(cancel)
			type candidateGenerationOutcome struct {
				result *personfacts.GenerationResult
				err    error
			}
			generationDone := make(chan candidateGenerationOutcome, 1)
			generationCtx := context.WithValue(
				ctx, personFactOrganizationCandidateGateActorKey{}, true)
			go func() {
				result, applyErr := st.ApplyPersonFactGenerationContext(generationCtx, input, nil)
				generationDone <- candidateGenerationOutcome{result: result, err: applyErr}
			}()
			waitPersonFactEmploymentLockSignal(
				t, gate.queried, "generation did not finish its non-ID candidate query")

			type mutationOutcome struct {
				organization *Organization
				err          error
			}
			mutationDone := make(chan mutationOutcome, 1)
			go func() {
				mutated := candidate
				input := OrganizationInput{
					Name: candidate.Name, Kind: candidate.Kind,
					PrimaryDomain: candidate.PrimaryDomain,
				}
				retired := false
				switch mode {
				case "rename":
					input.Name = "Renamed Candidate Race Company"
					domain := "renamed-candidate-race.example"
					input.PrimaryDomain = &domain
				case "retire":
					retired = true
				case "newly-matching":
					mutated = contender
					input.Name = candidate.Name
					input.PrimaryDomain = candidate.PrimaryDomain
				}
				organization, replaceErr := st.ReplaceOrganizationContext(
					ctx, mutated.ID, mutated.Revision, input, retired)
				mutationDone <- mutationOutcome{organization: organization, err: replaceErr}
			}()

			var early *mutationOutcome
			requirements.Eventually(func() bool {
				select {
				case outcome := <-mutationDone:
					early = &outcome
					return true
				default:
					return personFactOrganizationPostgreSQLWaitingLockCount(t, st) >= 1
				}
			}, 5*time.Second, 10*time.Millisecond,
				"organization mutation neither completed nor waited for the fact snapshot")
			assertions.Nil(early,
				"organization mutation committed before the fact candidate snapshot")
			gate.release()

			var generation candidateGenerationOutcome
			select {
			case generation = <-generationDone:
			case <-ctx.Done():
				requirements.FailNow("fact generation did not finish", ctx.Err())
			}
			requirements.NoError(generation.err)
			requirements.NotNil(generation.result)
			requirements.Len(generation.result.Projections, 1)
			var mutation mutationOutcome
			if early != nil {
				mutation = *early
			} else {
				select {
				case mutation = <-mutationDone:
				case <-ctx.Done():
					requirements.FailNow("organization mutation did not finish", ctx.Err())
				}
			}
			requirements.NoError(mutation.err)
			requirements.NotNil(mutation.organization)
			employment, err := st.GetEmploymentContext(
				t.Context(), generation.result.Projections[0].RowID)
			requirements.NoError(err)
			assertions.Equal(candidate.ID, employment.OrganizationID)
		})
	}
}

func TestPersonFactEmploymentOrganizationTableLockWaitsForMergeMutation(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("PostgreSQL lock queues are required for the table-lock upgrade race")
	}
	requirements := require.New(t)
	assertions := assert.New(t)
	gate := newPersonFactOrganizationCrossedLockGate(2)
	st := newPersonFactOrganizationCrossedLockStore(t, dbURL, gate)
	personID := createTrackedPersonFactPerson(t, st, "candidate-upgrade-race")
	target := projectionTargetBySlug(t, st, "employment")
	survivor := createPersonFactOrganization(
		t, st, "Candidate Upgrade Survivor", "candidate-upgrade-survivor.example")
	losing := createPersonFactOrganization(
		t, st, "Candidate Upgrade Losing", "candidate-upgrade-losing.example")
	claim := personFactProjectionClaim(personID, target,
		`{"organization":{"name":"Candidate Upgrade Losing","domain":"candidate-upgrade-losing.example"},"title":"Engineer"}`,
		"candidate-upgrade-race")
	input := personFactProjectionInput(personID, "candidate-upgrade-race",
		[]personfacts.ProposedClaim{claim}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	mergeCtx := context.WithValue(
		ctx, personFactOrganizationCrossedLockActorKey{}, personFactOrganizationCrossedLockFirst)
	mergeDone := make(chan error, 1)
	go func() {
		_, mergeErr := st.MergeOrganizationsContext(
			mergeCtx, survivor.ID, survivor.Revision, losing.ID, losing.Revision)
		mergeDone <- mergeErr
	}()
	waitPersonFactEmploymentLockSignal(
		t, gate.firstPaused, "merge did not acquire both organization rows")

	type candidateGenerationOutcome struct {
		result *personfacts.GenerationResult
		err    error
	}
	generationDone := make(chan candidateGenerationOutcome, 1)
	go func() {
		result, applyErr := st.ApplyPersonFactGenerationContext(ctx, input, nil)
		generationDone <- candidateGenerationOutcome{result: result, err: applyErr}
	}()
	requirements.Eventually(func() bool {
		return personFactOrganizationPostgreSQLWaitingLockCount(t, st) >= 1
	}, 5*time.Second, 10*time.Millisecond,
		"fact generation did not queue behind the merge table lock")
	gate.release()

	select {
	case mergeErr := <-mergeDone:
		requirements.NoError(mergeErr,
			"organization merge must retry a table-lock upgrade deadlock")
	case <-ctx.Done():
		requirements.FailNow("organization merge did not finish", ctx.Err())
	}
	var generation candidateGenerationOutcome
	select {
	case generation = <-generationDone:
	case <-ctx.Done():
		requirements.FailNow("fact generation did not retry the table-lock deadlock", ctx.Err())
	}
	requirements.NoError(generation.err)
	requirements.NotNil(generation.result)
	requirements.Len(generation.result.Projections, 1)
	employment, err := st.GetEmploymentContext(
		t.Context(), generation.result.Projections[0].RowID)
	requirements.NoError(err)
	assertions.NotEqual(losing.ID, employment.OrganizationID)
	employer, err := st.GetOrganizationContext(t.Context(), employment.OrganizationID)
	requirements.NoError(err)
	assertions.Equal(OrganizationKindCompany, employer.Kind)
	assertions.Nil(employer.RetiredAt)
	assertions.Nil(employer.MergedIntoID)
}

func applyPersonFactCrossedOrganizationGenerations(
	t *testing.T, st *Store, gate *personFactOrganizationCrossedLockGate,
	firstInput, secondInput personfacts.GenerationInput,
) {
	t.Helper()
	requirements := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	firstCtx := context.WithValue(
		ctx, personFactOrganizationCrossedLockActorKey{}, personFactOrganizationCrossedLockFirst)
	results := make(chan error, 2)
	go func() {
		_, applyErr := st.ApplyPersonFactGenerationContext(firstCtx, firstInput, nil)
		results <- applyErr
	}()
	waitPersonFactEmploymentLockSignal(
		t, gate.firstPaused, "first generation did not acquire its first redirect root")
	go func() {
		_, applyErr := st.ApplyPersonFactGenerationContext(ctx, secondInput, nil)
		results <- applyErr
	}()
	requirements.Eventually(func() bool {
		return personFactOrganizationPostgreSQLWaitingLockCount(t, st) >= 1
	}, 5*time.Second, 10*time.Millisecond,
		"second generation did not wait behind the first organization row")
	gate.release()

	errs := make([]error, 0, 2)
	for range 2 {
		select {
		case applyErr := <-results:
			errs = append(errs, applyErr)
		case <-ctx.Done():
			requirements.FailNow("crossed organization-chain generations did not finish", ctx.Err())
		}
	}
	for _, applyErr := range errs {
		requirements.NoError(applyErr,
			"crossed organization-chain batches must share one global lock order")
	}
}

func crossedPersonFactOrganizationGeneration(
	t *testing.T, personID int64, target personfacts.TargetDescriptor,
	firstSource, secondSource *Organization, wantedFirstID int64, suffix string,
) personfacts.GenerationInput {
	t.Helper()
	requirements := require.New(t)
	for attempt := range 100 {
		claims := []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, fmt.Sprintf(
				`{"organization":{"id":%d,"name":%q,"domain":%q},"title":%q}`,
				firstSource.ID, firstSource.Name, *firstSource.PrimaryDomain,
				fmt.Sprintf("Engineer %s A %d", suffix, attempt)),
				fmt.Sprintf("crossed-%s-a-%d", suffix, attempt)),
			personFactProjectionClaim(personID, target, fmt.Sprintf(
				`{"organization":{"id":%d,"name":%q,"domain":%q},"title":%q}`,
				secondSource.ID, secondSource.Name, *secondSource.PrimaryDomain,
				fmt.Sprintf("Engineer %s B %d", suffix, attempt)),
				fmt.Sprintf("crossed-%s-b-%d", suffix, attempt)),
		}
		input := personFactProjectionInput(personID,
			fmt.Sprintf("crossed-%s-%d", suffix, attempt), claims, nil)
		prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), input, nil)
		requirements.NoError(err)
		preparedClaims := prepared.Claims()
		requirements.Len(preparedClaims, 2)
		var value personfacts.EmploymentValue
		requirements.NoError(json.Unmarshal(preparedClaims[0].Normalized.JSON, &value))
		requirements.NotNil(value.Organization.ID)
		claimKeys := make([]string, len(preparedClaims))
		for index := range preparedClaims {
			claimKeys[index], err = personfacts.ClaimKey(prepared.GenerationKey(), preparedClaims[index])
			requirements.NoError(err)
		}
		storedFirst := 0
		if claimKeys[1] < claimKeys[0] {
			storedFirst = 1
		}
		var storedValue personfacts.EmploymentValue
		requirements.NoError(json.Unmarshal(
			preparedClaims[storedFirst].Normalized.JSON, &storedValue))
		requirements.NotNil(storedValue.Organization.ID)
		if *value.Organization.ID == wantedFirstID &&
			*storedValue.Organization.ID == wantedFirstID {
			return input
		}
	}
	requirements.FailNow("could not construct crossed canonical claim order")
	return personfacts.GenerationInput{}
}

func createTrackedPersonFactPerson(t *testing.T, st *Store, suffix string) int64 {
	t.Helper()
	participantID, err := st.EnsureParticipant(
		suffix+"@example.invalid", "Crossed Organization Person", "example.invalid")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	return person.ID
}

type personFactOrganizationCrossedLockActorKey struct{}

const personFactOrganizationCrossedLockFirst = "first"

type personFactOrganizationCrossedLockGate struct {
	firstPaused  chan struct{}
	releaseFirst chan struct{}
	pauseAt      int
	lockCount    int
	mu           sync.Mutex
	pauseOnce    sync.Once
	releaseOnce  sync.Once
}

func newPersonFactOrganizationCrossedLockGate(
	pauseAt int,
) *personFactOrganizationCrossedLockGate {
	return &personFactOrganizationCrossedLockGate{
		firstPaused: make(chan struct{}), releaseFirst: make(chan struct{}), pauseAt: pauseAt,
	}
}

func (g *personFactOrganizationCrossedLockGate) pauseAfterOrganizationLock(ctx context.Context) error {
	if ctx.Value(personFactOrganizationCrossedLockActorKey{}) !=
		personFactOrganizationCrossedLockFirst {
		return nil
	}
	g.mu.Lock()
	g.lockCount++
	pause := g.lockCount == g.pauseAt
	g.mu.Unlock()
	if !pause {
		return nil
	}
	var wait bool
	g.pauseOnce.Do(func() {
		wait = true
		close(g.firstPaused)
	})
	if !wait {
		return nil
	}
	select {
	case <-g.releaseFirst:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *personFactOrganizationCrossedLockGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseFirst) })
}

func (g *personFactOrganizationCrossedLockGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.firstPaused = make(chan struct{})
	g.releaseFirst = make(chan struct{})
	g.lockCount = 0
	g.pauseOnce = sync.Once{}
	g.releaseOnce = sync.Once{}
}

type personFactOrganizationCrossedLockConnector struct {
	driver.Connector

	gate *personFactOrganizationCrossedLockGate
}

func (c *personFactOrganizationCrossedLockConnector) Connect(
	ctx context.Context,
) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &personFactOrganizationCrossedLockConn{Conn: conn, gate: c.gate}, nil
}

type personFactOrganizationCrossedLockConn struct {
	driver.Conn

	gate *personFactOrganizationCrossedLockGate
}

func (c *personFactOrganizationCrossedLockConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil || ctx.Value(personFactOrganizationCrossedLockActorKey{}) !=
		personFactOrganizationCrossedLockFirst ||
		!strings.Contains(strings.Join(strings.Fields(query), " "),
			"FROM organizations WHERE id = $1 FOR UPDATE") {
		return rows, err
	}
	return &personFactOrganizationCrossedLockRows{
		Rows: rows, ctx: ctx, gate: c.gate,
	}, nil
}

func (c *personFactOrganizationCrossedLockConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *personFactOrganizationCrossedLockConn) BeginTx(
	ctx context.Context, opts driver.TxOptions,
) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, errors.New("wrapped organization crossed-lock connection does not implement ConnBeginTx")
}

func (c *personFactOrganizationCrossedLockConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *personFactOrganizationCrossedLockConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *personFactOrganizationCrossedLockConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *personFactOrganizationCrossedLockConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type personFactOrganizationCrossedLockRows struct {
	driver.Rows

	ctx  context.Context
	gate *personFactOrganizationCrossedLockGate
}

func (r *personFactOrganizationCrossedLockRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err != nil {
		return err
	}
	return r.gate.pauseAfterOrganizationLock(r.ctx)
}

func newPersonFactOrganizationCrossedLockStore(
	t *testing.T, dbURL string, gate *personFactOrganizationCrossedLockGate,
) *Store {
	t.Helper()
	base := newPGStoreInternal(t, dbURL)
	config, err := postgresConnConfig(base.dbPath, false)
	require.NoError(t, err)
	db := sql.OpenDB(&personFactOrganizationCrossedLockConnector{
		Connector: stdlib.GetConnector(*config), gate: gate,
	})
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	dialect := &PostgreSQLDialect{}
	require.NoError(t, dialect.InitConn(db))
	st := &Store{db: newLoggedDB(db, dialect.Rebind), dbPath: base.dbPath, dialect: dialect}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func personFactOrganizationPostgreSQLWaitingLockCount(t *testing.T, st *Store) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&count))
	return count
}

var _ driver.Rows = (*personFactOrganizationCrossedLockRows)(nil)
var _ io.Closer = (*personFactOrganizationCrossedLockRows)(nil)

type personFactOrganizationCandidateGateActorKey struct{}

type personFactOrganizationCandidateGate struct {
	queried      chan struct{}
	releaseQuery chan struct{}
	pauseAt      int
	queryCount   int
	mu           sync.Mutex
	pauseOnce    sync.Once
	releaseOnce  sync.Once
}

func newPersonFactOrganizationCandidateGate(pauseAt int) *personFactOrganizationCandidateGate {
	return &personFactOrganizationCandidateGate{
		queried: make(chan struct{}), releaseQuery: make(chan struct{}), pauseAt: pauseAt,
	}
}

func (g *personFactOrganizationCandidateGate) pauseAfterCandidateQuery(
	ctx context.Context,
) error {
	g.mu.Lock()
	g.queryCount++
	pause := g.queryCount == g.pauseAt
	g.mu.Unlock()
	if !pause {
		return nil
	}
	var wait bool
	g.pauseOnce.Do(func() {
		wait = true
		close(g.queried)
	})
	if !wait {
		return nil
	}
	select {
	case <-g.releaseQuery:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *personFactOrganizationCandidateGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseQuery) })
}

type personFactOrganizationCandidateGateConnector struct {
	driver.Connector

	gate *personFactOrganizationCandidateGate
}

func (c *personFactOrganizationCandidateGateConnector) Connect(
	ctx context.Context,
) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &personFactOrganizationCandidateGateConn{Conn: conn, gate: c.gate}, nil
}

type personFactOrganizationCandidateGateConn struct {
	driver.Conn

	gate *personFactOrganizationCandidateGate
}

func (c *personFactOrganizationCandidateGateConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil || ctx.Value(personFactOrganizationCandidateGateActorKey{}) != true ||
		!strings.Contains(strings.Join(strings.Fields(query), " "),
			"SELECT o.id FROM organizations o WHERE o.kind = 'company'") {
		return rows, err
	}
	return &personFactOrganizationCandidateGateRows{
		Rows: rows, ctx: ctx, gate: c.gate,
	}, nil
}

func (c *personFactOrganizationCandidateGateConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *personFactOrganizationCandidateGateConn) BeginTx(
	ctx context.Context, opts driver.TxOptions,
) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, errors.New("wrapped organization candidate-gate connection does not implement ConnBeginTx")
}

func (c *personFactOrganizationCandidateGateConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *personFactOrganizationCandidateGateConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *personFactOrganizationCandidateGateConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *personFactOrganizationCandidateGateConn) CheckNamedValue(
	value *driver.NamedValue,
) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type personFactOrganizationCandidateGateRows struct {
	driver.Rows

	ctx  context.Context
	gate *personFactOrganizationCandidateGate
}

func (r *personFactOrganizationCandidateGateRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if !errors.Is(err, io.EOF) {
		return err
	}
	if pauseErr := r.gate.pauseAfterCandidateQuery(r.ctx); pauseErr != nil {
		return pauseErr
	}
	return err
}

func newPersonFactOrganizationCandidateGateStore(
	t *testing.T, dbURL string, gate *personFactOrganizationCandidateGate,
) *Store {
	t.Helper()
	base := newPGStoreInternal(t, dbURL)
	config, err := postgresConnConfig(base.dbPath, false)
	require.NoError(t, err)
	db := sql.OpenDB(&personFactOrganizationCandidateGateConnector{
		Connector: stdlib.GetConnector(*config), gate: gate,
	})
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	dialect := &PostgreSQLDialect{}
	require.NoError(t, dialect.InitConn(db))
	st := &Store{db: newLoggedDB(db, dialect.Rebind), dbPath: base.dbPath, dialect: dialect}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

var _ driver.Rows = (*personFactOrganizationCandidateGateRows)(nil)
var _ io.Closer = (*personFactOrganizationCandidateGateRows)(nil)
