package store

import "time"

// ActivitySubject selects a saved person or an unsaved participant cluster.
type ActivitySubject struct {
	PersonID       int64
	ParticipantIDs []int64
}

// ContactAggregate summarizes counted interactions across the complete history.
type ContactAggregate struct {
	FirstAt, LastAt, LastInboundAt, LastOutboundAt *time.Time
	Count                                          int
	Channels                                       []ActivityChannel
	Monthly                                        [12]int
	InitiatedByContact, InitiatedByOwner           int
}
