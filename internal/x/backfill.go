package x

import (
	"go.kenn.io/msgvault/internal/store"
	"math"
	"time"
)

// ArchiveSummary distinguishes a network-free preview from a durable job.
type ArchiveSummary struct {
	Preview               bool      `json:"preview"`
	JobID                 string    `json:"job_id,omitempty"`
	Username              string    `json:"username"`
	OwnQuery              string    `json:"own_query"`
	MentionsQuery         string    `json:"mentions_query"`
	After                 time.Time `json:"after"`
	Before                time.Time `json:"before"`
	CapMicros             *int64    `json:"cap_micros,omitempty"`
	RemainingMicros       *int64    `json:"remaining_micros,omitempty"`
	SpentMicros           int64     `json:"spent_micros"`
	ReservedMicros        int64     `json:"reserved_micros"`
	PageResults           int       `json:"page_results"`
	PageReservationMicros int64     `json:"page_reservation_micros"`
	TotalUnknown          bool      `json:"total_unknown"`
	OwnComplete           bool      `json:"own_complete"`
	MentionsComplete      bool      `json:"mentions_complete"`
}

func (imp *Importer) archiveSummary(job store.XArchiveJob, preview bool) (*ArchiveSummary, error) {
	post, user := imp.policy.PostReadPriceMicros, imp.policy.UserReadPriceMicros
	if post > math.MaxInt64-user {
		return nil, store.ErrXRequestCost
	}
	unit := post + user
	if unit > math.MaxInt64/100 {
		return nil, store.ErrXRequestCost
	}
	summary := &ArchiveSummary{Preview: preview, PageResults: 100, PageReservationMicros: unit * 100}
	describeArchiveJob(summary, job)
	return summary, nil
}

func describeArchiveJob(summary *ArchiveSummary, job store.XArchiveJob) {
	summary.JobID = job.ID
	summary.Username = job.Username
	summary.OwnQuery = job.Own.Query
	summary.MentionsQuery = job.Mentions.Query
	summary.After = job.After
	summary.Before = job.Before
	summary.SpentMicros = job.SpentMicros
	summary.ReservedMicros = job.ReservedMicros
	summary.OwnComplete = job.Own.Complete
	summary.MentionsComplete = job.Mentions.Complete
	summary.TotalUnknown = summary.Preview || !job.Complete || job.ReservedMicros > 0
	if job.ID != "" {
		cap := job.CapMicros
		remaining := max(int64(0), cap-job.SpentMicros-job.ReservedMicros)
		summary.CapMicros = &cap
		summary.RemainingMicros = &remaining
	}
}
