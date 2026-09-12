package repo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// TestProperty_库存三大不变式 是 04-testing-standard.md §3.2 对"核心交易类
// 组件"的强制要求：例子测试只能证明"我想到的这几种情形是对的"，测不出
// "只在某个特定操作序列下才会触发"的边界 bug。这里用 rapid 生成随机的
// Receive/Reserve/ConfirmIssue/CancelReservation/Adjust 操作序列攻击同一个
// (product, warehouse)，每做完一步就查一次余额，断言三条不变式——不管
// 操作序列长什么样——永远成立：
//   1. on_hand_qty 永不为负
//   2. reserved_qty 永不为负
//   3. available（on_hand - reserved）永不为负——这条等价于"预留永远
//      不会超过在库"，是防超卖的最终判据
func TestProperty_库存三大不变式(t *testing.T) {
	db := testDB(t)
	east := warehouseID(t, db, "WH-EAST")
	allowed := allowedWarehouses(t, east)
	r := New(db, "erp_inventory_rw", "erp_inventory")

	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		pid := uniqueProductID("prop-inv")

		var openReservations []string
		n := rapid.IntRange(5, 25).Draw(rt, "opCount")
		for i := 0; i < n; i++ {
			op := rapid.SampledFrom([]string{"receive", "reserve", "adjust", "confirm", "cancel"}).Draw(rt, "op")
			switch op {
			case "receive":
				qty := rapid.IntRange(1, 500).Draw(rt, "receiveQty")
				if _, err := r.Receive(ctx, ReceiveInput{
					IdempotencyKey: fmt.Sprintf("prop-recv-%s-%d", pid, i),
					ProductID:      pid, WarehouseID: east, AllowedWarehouseIDs: allowed,
					Qty: strconv.Itoa(qty),
				}); err != nil {
					rt.Fatalf("Receive 不该失败（没有任何业务规则会拒绝正数入库）：%v", err)
				}

			case "reserve":
				qty := rapid.IntRange(1, 500).Draw(rt, "reserveQty")
				resID, err := r.Reserve(ctx, ReserveInput{
					IdempotencyKey: fmt.Sprintf("prop-rsv-%s-%d", pid, i),
					OrderID:        fmt.Sprintf("prop-order-%s-%d", pid, i),
					Items:          []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: strconv.Itoa(qty)}},
				})
				switch {
				case err == nil:
					openReservations = append(openReservations, resID)
				case errors.Is(err, ErrInsufficientStock):
					// 库存不够本来就该拒绝，不是缺陷。
				default:
					rt.Fatalf("Reserve 失败，但不是库存不足：%v", err)
				}

			case "adjust":
				delta := rapid.IntRange(-500, 500).Draw(rt, "adjustDelta")
				if _, err := r.Adjust(ctx, AdjustInput{
					IdempotencyKey: fmt.Sprintf("prop-adj-%s-%d", pid, i),
					ProductID:      pid, WarehouseID: east, AllowedWarehouseIDs: allowed,
					QtyDelta: strconv.Itoa(delta), Reason: "property test",
				}); err != nil && !errors.Is(err, ErrInsufficientStock) {
					rt.Fatalf("Adjust 失败，但不是库存不足：%v", err)
				}

			case "confirm":
				if len(openReservations) == 0 {
					continue
				}
				idx := rapid.IntRange(0, len(openReservations)-1).Draw(rt, "confirmIdx")
				resID := openReservations[idx]
				openReservations = append(openReservations[:idx], openReservations[idx+1:]...)
				if _, _, err := r.ConfirmIssue(ctx, ConfirmIssueInput{
					IdempotencyKey: fmt.Sprintf("prop-confirm-%s-%d", pid, i),
					ReservationID:  resID,
				}); err != nil {
					rt.Fatalf("ConfirmIssue 不该失败（这是一条真实存在的 RESERVED 预留）：%v", err)
				}

			case "cancel":
				if len(openReservations) == 0 {
					continue
				}
				idx := rapid.IntRange(0, len(openReservations)-1).Draw(rt, "cancelIdx")
				resID := openReservations[idx]
				openReservations = append(openReservations[:idx], openReservations[idx+1:]...)
				if _, err := r.CancelReservation(ctx, CancelReservationInput{
					IdempotencyKey: fmt.Sprintf("prop-cancel-%s-%d", pid, i),
					ReservationID:  resID,
				}); err != nil {
					rt.Fatalf("CancelReservation 不该失败：%v", err)
				}
			}

			b, err := r.GetBalance(ctx, pid, east, allowed)
			if err != nil {
				rt.Fatalf("GetBalance 失败：%v", err)
			}
			onHand, e1 := strconv.ParseFloat(b.OnHandQty, 64)
			reserved, e2 := strconv.ParseFloat(b.ReservedQty, 64)
			available, e3 := strconv.ParseFloat(b.AvailableQty, 64)
			if e1 != nil || e2 != nil || e3 != nil {
				rt.Fatalf("余额字段解析失败：%+v", b)
			}
			if onHand < 0 {
				rt.Fatalf("不变式违反：on_hand_qty=%v < 0（第 %d 步 %s 之后）", onHand, i, op)
			}
			if reserved < 0 {
				rt.Fatalf("不变式违反：reserved_qty=%v < 0（第 %d 步 %s 之后）", reserved, i, op)
			}
			if available < 0 {
				rt.Fatalf("不变式违反：available=on_hand-reserved=%v < 0，即预留超过在库——超卖（第 %d 步 %s 之后）", available, i, op)
			}
		}
	})
}

// TestProperty_Reserve并发同key仅执行一次 补的是 04-testing-standard.md §3.2
// 明确要求、但此前项目里从没写过的那一半幂等性测试："两个并发请求带着
// 同一个 idempotency_key 同时到达"，而不是"串行重放同一个 key 两次"——
// 已有的 TestReserve_并发防超卖 测的是另一件事（N 个不同 key 抢同一批
// 库存，验证不超卖），从没验证过"同一个 key 并发到达时是不是真的只执行
// 一次"。用 rapid 随机出并发数与预留数量，重复攻击 claimIdempotency 的
// `INSERT ... ON CONFLICT DO NOTHING` 那条判据。
func TestProperty_Reserve并发同key仅执行一次(t *testing.T) {
	db := testDB(t)
	db.SetMaxOpenConns(50) // 让并发请求真的用上不同物理连接，见 TestReserve_并发防超卖 同款注释
	east := warehouseID(t, db, "WH-EAST")
	allowed := allowedWarehouses(t, east)
	r := New(db, "erp_inventory_rw", "erp_inventory")

	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		pid := uniqueProductID("prop-idem")

		if _, err := r.Receive(ctx, ReceiveInput{
			IdempotencyKey: "prop-idem-seed-" + pid, ProductID: pid, WarehouseID: east,
			AllowedWarehouseIDs: allowed, Qty: "1000",
		}); err != nil {
			rt.Fatalf("准备库存失败：%v", err)
		}

		concurrency := rapid.IntRange(2, 20).Draw(rt, "concurrency")
		qty := rapid.IntRange(1, 100).Draw(rt, "qty")
		key := "prop-idem-key-" + pid

		results := make([]string, concurrency)
		errs := make([]error, concurrency)
		var wg sync.WaitGroup
		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			i := i
			go func() {
				defer wg.Done()
				results[i], errs[i] = r.Reserve(ctx, ReserveInput{
					IdempotencyKey: key,
					OrderID:        "prop-idem-order-" + pid,
					Items:          []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: strconv.Itoa(qty)}},
				})
			}()
		}
		wg.Wait()

		var first string
		for i, err := range errs {
			if err != nil {
				rt.Fatalf("并发 Reserve 用同一个 idempotency_key，goroutine %d 不该报错：%v", i, err)
			}
			if first == "" {
				first = results[i]
			} else if results[i] != first {
				rt.Fatalf("幂等性被打破：同一个 idempotency_key 的 %d 次并发调用应返回同一个 reservation_id，实际出现 %q 与 %q 两个不同的值",
					concurrency, first, results[i])
			}
		}

		b, err := r.GetBalance(ctx, pid, east, allowed)
		if err != nil {
			rt.Fatalf("GetBalance 失败：%v", err)
		}
		reserved, err := strconv.ParseFloat(b.ReservedQty, 64)
		if err != nil {
			rt.Fatalf("reserved_qty 解析失败：%q", b.ReservedQty)
		}
		if reserved != float64(qty) {
			rt.Fatalf("幂等性被打破：%d 次并发用同一个 key 调 Reserve(qty=%d)，reserved_qty 期望恰好 %d，实际 %v（多了说明重复执行）",
				concurrency, qty, qty, reserved)
		}
	})
}
