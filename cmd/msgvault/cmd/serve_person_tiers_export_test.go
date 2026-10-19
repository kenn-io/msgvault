package cmd

import (
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func PersonTierDaemonStoreForTest(st *store.Store) api.MessageStore {
	return &storeAPIAdapter{store: st}
}
