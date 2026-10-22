package inventory

import (
	"context"
	"sort"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/ids"
)

// key is one product in one warehouse: the unit of availability.
type key struct{ product, warehouse string }

// lockPositions makes sure each pair has a position row, then locks them all in (product, warehouse) order, the one
// order every transaction uses (RS16). It returns the positions before any change.
func lockPositions(ctx context.Context, tx db.Tx, legalEntity string, keys []key) (map[key]Position, error) {
	uniq := map[key]bool{}
	var ks []key
	for _, k := range keys {
		if !uniq[k] {
			uniq[k] = true
			ks = append(ks, k)
		}
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].product != ks[j].product {
			return ks[i].product < ks[j].product
		}
		return ks[i].warehouse < ks[j].warehouse
	})
	out := map[key]Position{}
	for _, k := range ks {
		if _, err := tx.Exec(ctx, `INSERT INTO stock_position (id, product_id, warehouse_id, legal_entity_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT (product_id, warehouse_id) DO NOTHING`, ids.New().String(), k.product,
			k.warehouse, legalEntity); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM stock_position WHERE product_id = $1 AND warehouse_id = $2 FOR UPDATE`,
			k.product, k.warehouse); err != nil {
			return nil, err
		}
		p, err := getPosition(ctx, tx, k.product, k.warehouse)
		if err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, nil
}

// moveStock adds on hand and reserved (either may be negative) to a locked position and publishes its new state.
func (m *Module) moveStock(ctx context.Context, tx db.Tx, k key, onHand, reserved string) error {
	if _, err := tx.Exec(ctx, `UPDATE stock_position SET on_hand = on_hand + $3::numeric, reserved = reserved + $4::numeric,
		version = version + 1, updated_at = now() WHERE product_id = $1 AND warehouse_id = $2`, k.product, k.warehouse,
		onHand, reserved); err != nil {
		return err
	}
	p, err := getPosition(ctx, tx, k.product, k.warehouse)
	if err != nil {
		return err
	}
	return m.emit(ctx, tx, PositionChanged, p.ID, p.Version, p)
}

// neg is the negative of a decimal string.
func neg(q string) string { return "-" + q }

// atLeast reports whether a >= b for decimal strings, in the database (no float anywhere).
func atLeast(ctx context.Context, tx db.Tx, a, b string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT $1::numeric >= $2::numeric`, a, b).Scan(&ok)
	return ok, err
}
