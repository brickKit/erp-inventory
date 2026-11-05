package inventory

import (
	"context"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/recovery"
)

// RecoveryChecks registers what a full recovery asks this member (be-protocol chapter 14): whether the objects this
// member audits are still here, at which version, and the checks it reports.
func (m *Module) RecoveryChecks(c *recovery.Checks) error {
	c.Presence(Warehouses, recovery.TablePresence("warehouse", "id", "version"))
	// a movement never changes once written: present or absent, no version
	c.Presence(Movements, func(ctx context.Context, tx db.Tx, ids []string) (map[string]int64, error) {
		rows, err := tx.Query(ctx, "SELECT id::text FROM movement WHERE id::text = ANY($1)", ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out[id] = 0
		}
		return out, rows.Err()
	})
	return nil
}
