package store_test

import (
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func init() {
	store.SetPGTestStoreForTest(testutil.NewTestStore)
}
