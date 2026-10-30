package store

import "context"

// A separate seed reaches archives whose base service catalog is already
// complete without overwriting operator edits on subsequent setup runs.
func (s *Store) seedThreadsService(ctx context.Context) error {
	return s.seedCommunicationServiceSet(ctx, "communication_services_threads_v1", []CommunicationServiceInput{{
		Slug: "threads", DisplayLabel: "Threads", ScopePolicy: ScopePolicyNone,
		Normalization: NormalizationStripAtLower, NormalizationVersion: 1,
		ProfileURLTemplate: new("https://www.threads.com/@{username}"),
	}})
}
