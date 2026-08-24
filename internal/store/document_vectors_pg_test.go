package store_test

import (
	"os"
	"testing"

	"go.kenn.io/msgvault/internal/store"
)

func TestDocumentVectorChunkLifecyclePostgreSQLContract(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL contract runs when MSGVAULT_TEST_DB selects PostgreSQL")
	}
	runDocumentVectorChunkLifecycleContract(t)
}

func TestDocumentVectorGenerationLifecyclePostgreSQLContract(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL contract runs when MSGVAULT_TEST_DB selects PostgreSQL")
	}
	runDocumentVectorGenerationLifecycleContract(t)
}
