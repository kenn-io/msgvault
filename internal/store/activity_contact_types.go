package store

import (
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

// ActivityFilters selects the stored evidence admitted to an activity result.
type ActivityFilters struct {
	Exclusions   persontiers.Config
	MaxDirect    int
	IncludeNoise bool
	// maxDirect is the archive projection policy for an omitted MaxDirect.
	maxDirect int
}

// ActivitySubject selects a saved person or an unsaved participant cluster.
type ActivitySubject struct {
	PersonID       int64
	ParticipantIDs []int64
}

// ContactAggregate summarizes counted interactions across the complete history.
type ContactAggregate struct {
	FirstAt, LastAt, LastInboundAt, LastOutboundAt  *time.Time
	FirstAtPrecision, LastAtPrecision               string
	LastInboundAtPrecision, LastOutboundAtPrecision string
	Count                                           int
	Channels                                        []ActivityChannel
	Monthly                                         [12]int
	InitiatedByContact, InitiatedByOwner            int
}

// ContactQuery supplies the identity, filters, clock and zone for an aggregate.
type ContactQuery struct {
	Subject  ActivitySubject
	Filters  ActivityFilters
	Now      time.Time
	Location *time.Location
}
