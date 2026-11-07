//go:build integration

package itest

import (
	"strconv"
	"time"

	"github.com/brickKit/be-sdk-go/v3/ids"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// participant: the cells of protocol 04's branch transitions the business flows do not reach, each answered as it is
// today (the reservation-participant plan's R0): a confirm or a cancel on a branch that ended changes nothing; only a
// committed branch is issued from (the refusal is a problem of the issue, not an error), never beyond what is left,
// and issuing all of it makes it consumed; a consumption ID issued again with other content is a conflict; a query
// leaves no tombstone; the participant's own job expires a hold past its period; an adjustment is not in this version.
// What is available afterwards is what it was less what was issued. It runs last, on stock of its own.
func (fx *fixture) participant() {
	t, ctx := fx.t, fx.ctx
	code, r := fx.alice.do("POST", "/api/inventory/receipts", map[string]any{"warehouse_id": fx.whSH, "business_date": "2026-10-22",
		"lines": []map[string]any{{"product_id": fx.rice, "quantity": "10"}}})
	require.Equal(t, 200, code, r)
	available := func() string { return fx.position(fx.rice, fx.whSH)["available"].(string) }
	before, onHand := available(), fx.position(fx.rice, fx.whSH)["on_hand"].(string)
	confirm := func(id string) (*inventoryv3.Branch, error) {
		r, err := fx.res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: id})
		return r.GetBranch(), err
	}
	cancel := func(id string) *inventoryv3.Branch {
		t.Helper()
		r, err := fx.res.Cancel(ctx, &inventoryv3.CancelRequest{BranchId: id})
		require.NoError(t, err)
		return r.GetBranch()
	}
	issue := func(id, consumption, qty string) (*inventoryv3.IssueResponse, error) {
		return fx.stockc.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: consumption, BranchId: id, BusinessDate: "2026-10-22",
			Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-R0"},
			Lines:     []*inventoryv3.IssueLine{{ProductId: fx.rice, WarehouseId: fx.whSH, Quantity: qty}}})
	}
	problem := func(is *inventoryv3.IssueResponse, err error, reason, detail string) {
		t.Helper()
		require.NoError(t, err)
		assert.False(t, is.Issued)
		require.Len(t, is.Problems, 1)
		assert.Equal(t, reason, is.Problems[0].Reason)
		assert.Equal(t, detail, is.Problems[0].Detail)
	}

	// a query leaves no tombstone: the hold after it is held
	fresh := ids.New().String()
	q, err := fx.res.Query(ctx, &inventoryv3.QueryRequest{BranchId: fresh})
	require.NoError(t, err)
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_ABSENT, q.Branch.State)
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_HELD, fx.hold(fresh, line(fx.rice, fx.whSH, "4")).State)

	// held: not issued from; confirmed twice, it stays committed
	is, err := issue(fresh, "R0-1", "1")
	problem(is, err, "branch_not_committed", "the branch is not committed: held")
	b, err := confirm(fresh)
	require.NoError(t, err)
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, b.State)
	b, err = confirm(fresh)
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, b.State, "confirming again answers committed")

	// committed: issued in parts, never beyond what is left; the same consumption ID with other content conflicts
	is, err = issue(fresh, "R0-1", "1")
	require.NoError(t, err)
	require.True(t, is.Issued, is.Problems)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, is.BranchState, "something is left")
	_, err = issue(fresh, "R0-1", "2")
	assert.Equal(t, codes.AlreadyExists, status.Code(err), err)
	assert.Contains(t, status.Convert(err).Message(), "consumption R0-1 was issued with other content")
	is, err = issue(fresh, "R0-2", "4")
	problem(is, err, "exceeds_branch", "more than the branch has left for this product and warehouse")
	is, err = issue(fresh, "R0-2", "3")
	require.NoError(t, err)
	require.True(t, is.Issued, is.Problems)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CONSUMED, is.BranchState, "all of it issued")

	// consumed: a confirm and a cancel change nothing; an issue is refused
	b, err = confirm(fresh)
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CONSUMED, b.State)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CONSUMED, cancel(fresh).State)
	is, err = issue(fresh, "R0-3", "1")
	problem(is, err, "branch_not_committed", "the branch is not committed: consumed")

	// released: a confirm and a cancel again change nothing
	released := ids.New().String()
	fx.hold(released, line(fx.rice, fx.whSH, "1"))
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_RELEASED, cancel(released).State)
	b, err = confirm(released)
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_RELEASED, b.State, "not revived")
	again := cancel(released)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_RELEASED, again.State)
	assert.Equal(t, b.Version, again.Version, "nothing changed")

	// cancelled (a tombstone): a confirm and a cancel change nothing
	tomb := ids.New().String()
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, cancel(tomb).State)
	b, err = confirm(tomb)
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, b.State)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, cancel(tomb).State)

	// rejected: more than there is; a confirm and a cancel change nothing
	over := ids.New().String()
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_REJECTED, fx.hold(over, line(fx.rice, fx.whSH, "1000")).State)
	b, err = confirm(over)
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_REJECTED, b.State)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_REJECTED, cancel(over).State)

	// expired by the participant's own job: a hold past its period; then confirm and cancel change nothing
	stale := ids.New().String()
	fx.hold(stale, line(fx.rice, fx.whSH, "1"))
	_, err = fx.admin.Exec(ctx, "UPDATE "+itSchema+".be_reserve_branch SET hold_expires_at = now() - interval '1 second' WHERE id = $1", stale)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		q, err := fx.res.Query(ctx, &inventoryv3.QueryRequest{BranchId: stale})
		return err == nil && q.Branch.State == inventoryv3.BranchState_BRANCH_STATE_EXPIRED
	}, 30*time.Second, 200*time.Millisecond, "the expire-holds job expires it")
	b, err = confirm(stale)
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_EXPIRED, b.State)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_EXPIRED, cancel(stale).State)

	// an adjustment (an order revision) is not in this version
	_, err = fx.res.Adjust(ctx, &inventoryv3.AdjustRequest{BranchId: stale})
	assert.Equal(t, codes.Unimplemented, status.Code(err), err)
	assert.Contains(t, status.Convert(err).Message(), "branch adjustment (order revisions) is not in this version of erp/inventory")

	// the 4 issued left the stock: on hand and available both 4 less than before the first hold; everything else came back
	was, _ := strconv.Atoi(onHand)
	assert.Equal(t, strconv.Itoa(was-4), fx.position(fx.rice, fx.whSH)["on_hand"], "4 issued")
	free, _ := strconv.Atoi(before)
	assert.Equal(t, strconv.Itoa(free-4), available(), "what was held came back or was issued")
}
