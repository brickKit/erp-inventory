//go:build integration

package itest

import (
	"sync"

	"github.com/brickKit/be-sdk-go/v3/ids"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// racingHolds: RS01: ten orders race for five kilograms of rice, one each: exactly five are held; RS16: two orders
// lock the same two positions from opposite ends: no deadlock, each all or nothing.
func (fx *fixture) racingHolds() {
	t := fx.t
	// RS01: ten orders race for five kilograms of rice in Shanghai, one kilogram each: exactly five are held
	var wg sync.WaitGroup
	var mu sync.Mutex
	states := map[inventoryv3.BranchState]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := fx.hold(ids.New().String(), line(fx.rice, fx.whSH, "1"))
			mu.Lock()
			states[b.State]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, 5, states[inventoryv3.BranchState_BRANCH_STATE_HELD], "no oversell: %v", states)
	assert.Equal(t, 5, states[inventoryv3.BranchState_BRANCH_STATE_REJECTED])
	assert.Equal(t, "0", fx.position(fx.rice, fx.whSH)["available"])

	// RS16: two orders lock the same two positions from opposite ends: no deadlock, each all or nothing
	var b1, b2 *inventoryv3.Branch
	wg.Add(2)
	go func() {
		defer wg.Done()
		b1 = fx.hold(ids.New().String(), line(fx.tea, fx.whSH, "3"), line(fx.rice, fx.whBJ, "3"))
	}()
	go func() {
		defer wg.Done()
		b2 = fx.hold(ids.New().String(), line(fx.rice, fx.whBJ, "3"), line(fx.tea, fx.whSH, "3"))
	}()
	wg.Wait()
	held := 0
	for _, b := range []*inventoryv3.Branch{b1, b2} {
		if b.State == inventoryv3.BranchState_BRANCH_STATE_HELD {
			held++
		} else {
			assert.Equal(t, "insufficient_stock", b.Reason)
			assert.NotEmpty(t, b.Shortages)
		}
	}
	assert.Equal(t, 1, held, "rice in Beijing has 5: only one of the two holds 3")
	assert.Equal(t, "7", fx.position(fx.tea, fx.whSH)["available"], "the refused hold reserved nothing, the fx.tea included")
}

// holdIdempotence: RS03: the answer to a hold is lost and the coordinator holds again: one reservation; RS17: the
// same branch ID with another quantity is refused; a warehouse of another legal entity is an invalid line.
func (fx *fixture) holdIdempotence() {
	t, ctx := fx.t, fx.ctx
	// RS03: the answer to a hold is lost and the coordinator holds again: one reservation
	order := ids.New().String()
	fx.order = order
	first := fx.hold(order, line(fx.tea, fx.whSH, "2"))
	again := fx.hold(order, line(fx.tea, fx.whSH, "2"))
	assert.Equal(t, first.Version, again.Version)
	assert.Equal(t, "5", fx.position(fx.tea, fx.whSH)["available"])
	// RS17: the same branch ID with another quantity is refused
	_, err := fx.res.Hold(ctx, &inventoryv3.HoldRequest{BranchId: order, LegalEntityId: fx.le,
		Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-" + order[:8], Revision: 1}, Lines: []*inventoryv3.HoldLine{line(fx.tea, fx.whSH, "1")}})
	assert.Equal(t, codes.AlreadyExists, status.Code(err), "CONTENT_CONFLICT")
	// a warehouse of another legal entity is an invalid line, not stock to take
	other := fx.hold(ids.New().String(), line(fx.tea, fx.whHK, "1"))
	assert.Equal(t, "invalid_line", other.Reason)
	assert.Equal(t, "warehouse_not_in_legal_entity", other.Shortages[0].Reason)
}

// lateAndExpired: RS04: the cancel reaches inventory before the hold: the tombstone refuses the late hold; an unknown
// branch is absent; an expired hold is not revived by a late confirm and gave its stock back.
func (fx *fixture) lateAndExpired() {
	t, ctx := fx.t, fx.ctx
	// RS04: the cancel reaches inventory before the hold: the tombstone refuses the late hold
	late := ids.New().String()
	c, err := fx.res.Cancel(ctx, &inventoryv3.CancelRequest{BranchId: late})
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, c.Branch.State)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, fx.hold(late, line(fx.tea, fx.whSH, "1")).State)
	assert.Equal(t, "5", fx.position(fx.tea, fx.whSH)["available"])
	q, err := fx.res.Query(ctx, &inventoryv3.QueryRequest{BranchId: ids.New().String()})
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_ABSENT, q.Branch.State)

	// an expired hold is not revived by a late confirm
	stale := ids.New().String()
	fx.hold(stale, line(fx.tea, fx.whSH, "1"))
	_, err = fx.admin.Exec(ctx, "UPDATE "+itSchema+".be_reserve_branch SET hold_expires_at = now() - interval '1 second' WHERE id = $1", stale)
	require.NoError(t, err)
	cf, err := fx.res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: stale})
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_EXPIRED, cf.Branch.State)
	assert.Equal(t, "5", fx.position(fx.tea, fx.whSH)["available"], "the expired hold gave its stock back")
}
