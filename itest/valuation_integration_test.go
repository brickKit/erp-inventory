//go:build integration

package itest

import (
	"encoding/json"

	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// valuationRead: the costing component reads a legal entity's movements of one product by their valuation sequence
// (protocol 05b, "计算与缺口": the gap refetch), in order, a page at a time, with the last number of the pair; the
// costing component only.
func (fx *fixture) valuationRead() {
	t := fx.t
	var epoch string
	var first, last int64
	require.NoError(t, fx.admin.QueryRow(fx.ctx, "SELECT valuation_epoch, min(valuation_seq), max(valuation_seq) FROM "+
		itSchema+".movement WHERE legal_entity_id = $1 AND product_id = $2 AND valuation_seq IS NOT NULL GROUP BY 1 "+
		"ORDER BY 1 DESC LIMIT 1", fx.le, fx.tea).Scan(&epoch, &first, &last))
	require.Greater(t, last, first, "the tea has at least two numbered movements")
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, "erp/costing").ClientConfig("erp/inventory"))))
	require.NoError(t, err)
	defer conn.Close()
	costing := inventoryv3.NewStockServiceClient(conn)

	page, err := costing.Movements(fx.ctx, &inventoryv3.MovementsRequest{LegalEntityId: fx.le, ProductId: fx.tea,
		ValuationEpoch: epoch, FromSeq: first, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.GetMovements(), 2)
	assert.Equal(t, last, page.GetLastSeq())
	var seqs []int64
	for _, raw := range page.GetMovements() {
		var mv map[string]any
		require.NoError(t, json.Unmarshal(raw, &mv))
		assert.Equal(t, fx.tea, mv["product_id"])
		assert.Equal(t, fx.le, mv["legal_entity_id"])
		vs := mv["valuation_seq"].(map[string]any)
		assert.Equal(t, epoch, vs["epoch"])
		seqs = append(seqs, int64(vs["seq"].(float64)))
	}
	assert.Equal(t, []int64{first, first + 1}, seqs, "in their order, from the first asked")

	page, err = costing.Movements(fx.ctx, &inventoryv3.MovementsRequest{LegalEntityId: fx.le, ProductId: fx.tea,
		ValuationEpoch: epoch, FromSeq: last})
	require.NoError(t, err)
	assert.Len(t, page.GetMovements(), 1, "the last one")
	page, err = costing.Movements(fx.ctx, &inventoryv3.MovementsRequest{LegalEntityId: fx.le, ProductId: fx.tea,
		ValuationEpoch: epoch, FromSeq: last + 1})
	require.NoError(t, err)
	assert.Empty(t, page.GetMovements(), "none past the last")
	assert.Equal(t, last, page.GetLastSeq())
	page, err = costing.Movements(fx.ctx, &inventoryv3.MovementsRequest{LegalEntityId: fx.le, ProductId: fx.tea,
		ValuationEpoch: "another", FromSeq: 1})
	require.NoError(t, err)
	assert.Empty(t, page.GetMovements(), "another epoch has none")
	assert.Zero(t, page.GetLastSeq())

	_, err = fx.stockc.Movements(fx.ctx, &inventoryv3.MovementsRequest{LegalEntityId: fx.le, ProductId: fx.tea,
		ValuationEpoch: epoch, FromSeq: first})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the costing component only: %v", err)
	_, err = costing.Movements(fx.ctx, &inventoryv3.MovementsRequest{LegalEntityId: "x", ProductId: fx.tea,
		ValuationEpoch: epoch, FromSeq: first})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
}
