package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Receipt is a confirmed idempotent operation result.
type Receipt struct {
	RequestHash      []byte
	Operation        string
	CommittedVersion *int64
	Result           []byte
}

// GetReceipt returns the unexpired receipt for (scope, actor, key).
func (t *Tx) GetReceipt(ctx context.Context, scope string, actorID int64, keyHash []byte, now time.Time) (*Receipt, error) {
	var r Receipt
	err := t.tx.QueryRow(ctx, `SELECT request_hash, operation, committed_version, result FROM operation_receipt
		WHERE scope = $1 AND actor_id = $2 AND key_hash = $3 AND expires_at > $4`, scope, actorID, keyHash, now).
		Scan(&r.RequestHash, &r.Operation, &r.CommittedVersion, &r.Result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // absence is a normal outcome
	}
	if err != nil {
		return nil, fmt.Errorf("reading receipt: %w", err)
	}
	return &r, nil
}

// PutReceipt stores a receipt in the same transaction as the effect. An
// expired receipt with the same key is replaced.
func (t *Tx) PutReceipt(ctx context.Context, scope string, actorID int64, keyHash []byte, r Receipt, now, expires time.Time) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM operation_receipt
		WHERE scope = $1 AND actor_id = $2 AND key_hash = $3 AND expires_at <= $4`, scope, actorID, keyHash, now); err != nil {
		return fmt.Errorf("replacing expired receipt: %w", err)
	}
	_, err := t.tx.Exec(ctx, `INSERT INTO operation_receipt
		(scope, actor_id, key_hash, request_hash, operation, committed_version, result, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		scope, actorID, keyHash, r.RequestHash, r.Operation, r.CommittedVersion, r.Result, now, expires)
	if err != nil {
		return fmt.Errorf("writing receipt: %w", err)
	}
	return nil
}

// PurgeExpiredReceipts deletes expired receipts in bounded batches (admin only).
func (s *Store) PurgeExpiredReceipts(ctx context.Context, now time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM operation_receipt WHERE id IN (
			SELECT id FROM operation_receipt WHERE expires_at <= $1 ORDER BY id LIMIT $2)`, now, batch)
		if err != nil {
			return total, fmt.Errorf("purging receipts: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}
