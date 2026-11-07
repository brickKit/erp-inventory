//go:build integration

package itest

import (
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	core "github.com/brickKit/erp-inventory/v3/internal/core"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// dimensions: Bob keeps the Beijing warehouse only: positions, branches and warehouses show only what he may see.
func (fx *fixture) dimensions() {
	t := fx.t
	// the dimensions: Bob keeps the Beijing warehouse only
	all := []string{"id", "product_id", "warehouse_id", "legal_entity_id", "on_hand", "reserved", "available", "version", "updated_at",
		"batch", "first_received_at", "serial", "in_stock"}
	fx.w.Grant(sectest.Bob, sdkauthz.Segment{ResourceType: core.Stock, Operation: core.OpStockRead, ReadFields: all,
		Atoms: map[string]sdkauthz.Atom{core.DimLegalEntity: {Kind: "all"}, core.DimWarehouse: {Kind: "in", Values: []string{fx.whBJ}}},
		Valid: sdkauthz.Window{From: "2026-01-01T00:00:00Z"}})
	bob := client{t, fx.w.Bearer(sectest.Bob)}
	var ps []any
	require.Eventually(t, func() bool { // the member takes the new grant with its next authorization view
		_, page := bob.do("GET", "/api/inventory/positions", nil)
		ps, _ = page["positions"].([]any)
		return len(ps) > 0
	}, 30*time.Second, 200*time.Millisecond)
	for _, p := range ps {
		assert.Equal(t, fx.whBJ, p.(map[string]any)["warehouse_id"])
	}
	code, _ := bob.do("GET", "/api/inventory/branches/"+fx.order, nil)
	assert.Equal(t, 404, code, "the branch is in Shanghai: Bob cannot see it")
	code, whs := bob.do("GET", "/api/inventory/warehouses", nil)
	assert.Equal(t, 200, code)
	assert.Empty(t, whs["warehouses"], "no warehouse read was granted: a list shows only what the caller may see")
}
