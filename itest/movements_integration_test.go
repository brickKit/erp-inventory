//go:build integration

package itest

import (
	"encoding/json"
	"time"

	core "github.com/brickKit/erp-inventory/v3/internal/core"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// receipts: batches, serial numbers, plain stock; the tracking policy decides what a line must name; a service is
// not stocked.
func (fx *fixture) receipts() {
	t := fx.t
	// receipts: batches, serial numbers, plain stock; the tracking policy decides what a line must name
	receive := func(wh string, lines ...map[string]any) (int, map[string]any) {
		return fx.alice.do("POST", "/api/inventory/receipts", map[string]any{"warehouse_id": wh, "business_date": "2026-10-20", "lines": lines})
	}
	code, r := receive(fx.whSH, map[string]any{"product_id": fx.tea, "quantity": "4", "batch": "T-OLD"})
	require.Equal(t, 200, code, r)
	code, r = receive(fx.whSH, map[string]any{"product_id": fx.tea, "quantity": "6", "batch": "T-NEW"},
		map[string]any{"product_id": fx.rice, "quantity": "5.000"},
		map[string]any{"product_id": fx.phone, "quantity": "3", "serials": []string{"SN-1", "SN-2", "SN-3"}})
	require.Equal(t, 200, code, r)
	_, _ = receive(fx.whBJ, map[string]any{"product_id": fx.rice, "quantity": "5"})
	code, r = receive(fx.whSH, map[string]any{"product_id": fx.tea, "quantity": "1"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "TRACKING_MISMATCH", r["reason"], "a batch-tracked receipt names its batch")
	code, r = receive(fx.whBJ, map[string]any{"product_id": fx.phone, "quantity": "1", "serials": []string{"SN-2"}})
	assert.Equal(t, 409, code)
	assert.Equal(t, "TRACKING_MISMATCH", r["reason"], "a serial number in stock is not received again")
	code, r = receive(fx.whSH, map[string]any{"product_id": fx.svc, "quantity": "1"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "PRODUCT_NOT_STOCKED", r["reason"])

	assert.Equal(t, "10", fx.position(fx.tea, fx.whSH)["on_hand"])
	assert.Equal(t, "5", fx.position(fx.rice, fx.whSH)["available"])
}

// numbering: the movement events carry both sequences: numbered after commit, the event written in the valuation
// numbering; the inventory sequence has no gap; the valuation sequence counts per product and legal entity.
func (fx *fixture) numbering() {
	t, ctx := fx.t, fx.ctx
	// the movement events carry both sequences: numbered after commit, the event written in the valuation numbering
	require.Eventually(t, func() bool {
		var n int
		_ = fx.admin.QueryRow(ctx, "SELECT count(*) FROM "+itSchema+".movement WHERE valuation_seq IS NULL").Scan(&n)
		return n == 0
	}, 10*time.Second, 100*time.Millisecond)
	var seqs []int64
	rows, err := fx.admin.Query(ctx, "SELECT inventory_seq FROM "+itSchema+".movement ORDER BY inventory_seq")
	require.NoError(t, err)
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		seqs = append(seqs, s)
	}
	rows.Close()
	for i, s := range seqs {
		assert.Equal(t, int64(i+1), s, "the inventory sequence has no gap")
	}
	var payload []byte
	require.NoError(t, fx.admin.QueryRow(ctx, "SELECT payload FROM "+itSchema+".be_outbox WHERE type = $1 ORDER BY ord DESC LIMIT 1",
		core.MovementRecorded).Scan(&payload))
	var ev core.Movement
	require.NoError(t, json.Unmarshal(payload, &ev))
	assert.NotNil(t, ev.InventorySeq)
	assert.NotNil(t, ev.ValuationSeq)
	var valTea []int64
	rows, err = fx.admin.Query(ctx, "SELECT valuation_seq FROM "+itSchema+".movement WHERE product_id = $1 ORDER BY valuation_seq", fx.tea)
	require.NoError(t, err)
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		valTea = append(valTea, s)
	}
	rows.Close()
	assert.Equal(t, []int64{1, 2, 3}, valTea, "the valuation sequence counts per product and legal entity")
}
