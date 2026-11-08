package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestImportantDateAmbiguousSplitNeedsReviewedWithdrawnFacts(t *testing.T) {
	must := require.New(t)
	lineage := ImportantDateLineage{SplitResults: []PersonSplitResult{{Split: PersonSplit{ID: 42}, AmbiguousRows: []PersonMergeRowRef{{TableName: "person_dates"}}}}}
	must.Error(validateImportantDateSplitEvidence(lineage, nil))
	must.Error(validateImportantDateSplitEvidence(lineage, []int64{41}))
	must.NoError(validateImportantDateSplitEvidence(lineage, []int64{42}))
	lineage.People = []ImportantDateLineagePerson{{Dates: []PersonDate{{DateKind: PersonDateBirthday, Envelope: ValueEnvelope{Source: ProvenanceUser}}}}}
	must.Error(validateImportantDateSplitEvidence(lineage, []int64{42}))
	lineage.People[0].Dates[0].Envelope.SupersededAt = new(time.Now())
	must.NoError(validateImportantDateSplitEvidence(lineage, []int64{42}))
	lineage.People[0].ContactFrequency = []PersonAttributeValue{{}}
	must.Error(validateImportantDateSplitEvidence(lineage, []int64{42}))
	lineage.People[0].ContactFrequency = nil
	lineage.People[0].Relationships = []PersonRelationshipView{{Relationship: PersonRelationship{TypeSlug: "parent", Status: RelationshipStatusActive}}}
	must.Error(validateImportantDateSplitEvidence(lineage, []int64{42}))
	lineage.People[0].Relationships = nil
	lineage.OwnedSubjects = []ImportantDateLineageSubject{{UID: "subject-example"}}
	must.Error(validateImportantDateSplitEvidence(lineage, []int64{42}))
	lineage.OwnedSubjects[0].Withdrawn = true
	must.NoError(validateImportantDateSplitEvidence(lineage, []int64{42}))
}
