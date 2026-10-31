package cmd

import "go.kenn.io/msgvault/internal/timeline"

func (a *storeAPIAdapter) TimelineSources() (timeline.Sources, error) {
	return timeline.NewStoreSources(a.store, a.config)
}
