package movements

import (
	"context"
	"encoding/json"

	core "github.com/brickKit/erp-inventory/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/db"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
)

// ByValuation is StockService.Movements (the costing component's gap refetch, protocol 05b): one legal entity's
// numbered movements of one product in a valuation epoch from a sequence number on, in their order, each as its event
// carries it, with the highest number of the pair in the epoch.
func ByValuation(m *core.Base, ctx context.Context, in *inventoryv3.MovementsRequest) (*inventoryv3.MovementsResponse, error) {
	if !core.UUIDForm.MatchString(in.GetLegalEntityId()) {
		return nil, core.Invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	}
	if !core.UUIDForm.MatchString(in.GetProductId()) {
		return nil, core.Invalid("product_id", "product_id is a product's ID")
	}
	limit := int(in.GetLimit())
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 1000)
	out := &inventoryv3.MovementsResponse{}
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(valuation_seq), 0) FROM movement WHERE legal_entity_id = $1
			AND product_id = $2 AND valuation_epoch = $3`, in.GetLegalEntityId(), in.GetProductId(),
			in.GetValuationEpoch()).Scan(&out.LastSeq); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+core.MovementCols+` FROM movement m WHERE m.legal_entity_id = $1
			AND m.product_id = $2 AND m.valuation_epoch = $3 AND m.valuation_seq >= $4 ORDER BY m.valuation_seq LIMIT $5`,
			in.GetLegalEntityId(), in.GetProductId(), in.GetValuationEpoch(), in.GetFromSeq(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			mv, err := core.ScanMovement(rows)
			if err != nil {
				return err
			}
			b, err := json.Marshal(mv)
			if err != nil {
				return err
			}
			out.Movements = append(out.Movements, b)
		}
		return rows.Err()
	})
	return out, err
}
