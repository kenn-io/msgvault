//go:build sqlite_vec

package cmd

import (
	"fmt"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/operations"
)

var testCLIEmbeddingPassSequence atomic.Uint64

func testCLIEmbeddingPassScope() operations.PassScope {
	sequence := testCLIEmbeddingPassSequence.Add(1)
	return operations.PassScope{
		Key: fmt.Sprintf("test:cli-embedding:%d", sequence), Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 0, 0, 0, int(sequence), time.UTC),
	}
}
