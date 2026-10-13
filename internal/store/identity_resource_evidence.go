package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
)

// ErrIdentityOperationTooLarge rejects an affected set that cannot fit an explicit resource grant.
var ErrIdentityOperationTooLarge = errors.New("identity operation affects too many resources")

type IdentityPersonEvidence struct {
	ID                 int64  `json:"id"`
	UID                string `json:"uid"`
	Revision           int64  `json:"revision"`
	ProjectionRevision int64  `json:"projection_revision"`
}

func canonicalIdentityIDs(ids []int64) []int64 {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func identityReadRows(ctx context.Context, tx *loggedTx, query string, args []any, read func(*loggedRows) error) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := read(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func identityEvidenceHash(value any) (string, error) {
	b, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
