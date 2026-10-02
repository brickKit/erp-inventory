package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
	"github.com/brickKit/erp-inventory/v2/backend/internal/service"
)

// 这里测的是 REST 层把查询参数接对了、响应体形状对：handler 直接挂在测试自己的
// engine 上，不经过权限中间件（权限判定是 SDK 的事，由 SDK 自己的测试守）；
// 请求的 ctx 里放一份已验签的 Claims，service 层照常按 warehouse_access 过滤。

const (
	testRole   = "erp_inventory_rw"
	testSchema = "erp_inventory"
)

func testRepo(t *testing.T) (*repo.Repo, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return repo.New(db, testRole, testSchema), db
}

var seq int64

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddInt64(&seq, 1))
}

func warehouseID(t *testing.T, db *sql.DB, code string) string {
	t.Helper()
	var id int64
	err := besdk.WithTx(context.Background(), db, testRole, testSchema, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT id FROM warehouses WHERE code = $1`, code).Scan(&id)
	})
	if err != nil {
		t.Fatalf("查种子仓库 %q：%v", code, err)
	}
	return strconv.FormatInt(id, 10)
}

// handlerErrorCode 发一次 GET，返回 handler 用 c.Error 交出的 gRPC 错误码（没有
// 错误时是 codes.OK）。
func handlerErrorCode(t *testing.T, path string, h gin.HandlerFunc, sub string, query url.Values) codes.Code {
	t.Helper()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	got := codes.OK
	eng.Use(func(c *gin.Context) {
		c.Next()
		if len(c.Errors) > 0 {
			got = status.Code(c.Errors.Last().Err)
		}
	})
	eng.GET(path, h)
	req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	req = req.WithContext(besdk.ContextWithClaims(req.Context(), besdk.Claims{Sub: sub}))
	eng.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// serve 发一次 GET，返回状态码与响应体；ctx 里带 sub 的 Claims。
func serve(t *testing.T, path string, h gin.HandlerFunc, sub string, query url.Values) (int, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.GET(path, h)
	req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	req = req.WithContext(besdk.ContextWithClaims(req.Context(), besdk.Claims{Sub: sub}))
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

type movementsBody struct {
	Movements []struct {
		ID string `json:"id"`
	} `json:"movements"`
	NextCursor string `json:"next_cursor"`
}

// TestListMovements_REST按created_before收窄时间窗口：契约里 GET /movements 带
// created_after / created_before。刚写入的一条流水，created_before 给一小时前
// 时不该出现；不给时出现。
func TestListMovements_REST按created_before收窄时间窗口(t *testing.T) {
	r, db := testRepo(t)
	svc := service.New(r, slog.Default())
	ctx := context.Background()
	east := warehouseID(t, db, "WH-EAST")
	sub := unique("http-list-window")
	if err := r.GrantWarehouseAccess(ctx, sub, east); err != nil {
		t.Fatal(err)
	}
	eastID, _ := strconv.ParseInt(east, 10, 64)
	pid := unique("http-list-window")
	if _, err := r.Receive(ctx, repo.ReceiveInput{
		IdempotencyKey: pid, ProductID: pid, WarehouseID: east, Qty: "1",
		AllowedWarehouseIDs: []int64{eastID},
	}); err != nil {
		t.Fatal(err)
	}

	list := func(q url.Values) (int, movementsBody) {
		code, raw := serve(t, "/movements", listMovementsHandler(svc), sub, q)
		var body movementsBody
		if code == http.StatusOK {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("响应不是合法 JSON：%v（%s）", err, raw)
			}
		}
		return code, body
	}

	code, body := list(url.Values{"product_id": {pid}})
	if code != http.StatusOK || len(body.Movements) != 1 {
		t.Fatalf("不给时间窗口时应看到刚写入的 1 条流水，实际 %d / %d 条", code, len(body.Movements))
	}
	hourAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, body = list(url.Values{"product_id": {pid}, "created_before": {hourAgo}})
	if code != http.StatusOK {
		t.Fatalf("created_before=%s 应返回 200，实际 %d", hourAgo, code)
	}
	if len(body.Movements) != 0 {
		t.Fatalf("created_before=%s 时刚写入的流水不该出现，实际 %d 条", hourAgo, len(body.Movements))
	}
}

// TestListMovements_REST的created_after格式不对返回400：时间参数格式不对是入参
// 错误，不能悄悄忽略、按默认窗口返回。
func TestListMovements_REST的created_after格式不对返回400(t *testing.T) {
	r, _ := testRepo(t)
	svc := service.New(r, slog.Default())
	code, raw := serve(t, "/movements", listMovementsHandler(svc), unique("http-list-bad-time"),
		url.Values{"created_after": {"昨天"}})
	if code != http.StatusBadRequest {
		t.Fatalf("created_after=昨天 应返回 400，实际 %d（%s）", code, raw)
	}
}

// TestNewReadEndpoints_REST响应体形状：三个新读端点的 JSON 字段名与
// contracts/inventory.openapi.yaml 一致（前端与 BFF 按契约生成类型），数量字段是
// 十进制字符串；余额列表的非法游标回 400。数据范围本身由 service 层的测试守。
func TestNewReadEndpoints_REST响应体形状(t *testing.T) {
	r, db := testRepo(t)
	svc := service.New(r, slog.Default())
	ctx := context.Background()
	east := warehouseID(t, db, "WH-EAST")
	eastID, _ := strconv.ParseInt(east, 10, 64)
	sub := unique("http-shape")
	if err := r.GrantWarehouseAccess(ctx, sub, east); err != nil {
		t.Fatal(err)
	}
	pid := unique("http-shape")
	if _, err := r.Receive(ctx, repo.ReceiveInput{
		IdempotencyKey: pid, ProductID: pid, WarehouseID: east, Qty: "2", AllowedWarehouseIDs: []int64{eastID},
	}); err != nil {
		t.Fatal(err)
	}

	decode := func(name string, code int, raw []byte) map[string]any {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%s 应返回 200，实际 %d（%s）", name, code, raw)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s 响应不是 JSON 对象：%v（%s）", name, err, raw)
		}
		return m
	}
	requireKeys := func(name string, m map[string]any, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if _, ok := m[k]; !ok {
				t.Fatalf("%s 的响应缺字段 %q：%v", name, k, m)
			}
		}
	}

	code, raw := serve(t, "/warehouses", listWarehousesHandler(svc), sub, nil)
	m := decode("GET /warehouses", code, raw)
	requireKeys("GET /warehouses", m, "warehouses")
	whs, _ := m["warehouses"].([]any)
	if len(whs) != 1 {
		t.Fatalf("GET /warehouses 应只返回授权的 1 个仓库，实际 %v", m["warehouses"])
	}
	requireKeys("warehouses[0]", whs[0].(map[string]any), "id", "code", "name", "status")
	if whs[0].(map[string]any)["id"] != east {
		t.Fatalf("仓库 id 应是字符串 %q，实际 %v", east, whs[0].(map[string]any)["id"])
	}

	code, raw = serve(t, "/balances/list", listBalancesHandler(svc), sub, url.Values{"product_id": {pid}})
	m = decode("GET /balances/list", code, raw)
	requireKeys("GET /balances/list", m, "balances", "next_cursor")
	bals, _ := m["balances"].([]any)
	if len(bals) != 1 {
		t.Fatalf("GET /balances/list?product_id= 应返回 1 行，实际 %v", m["balances"])
	}
	b := bals[0].(map[string]any)
	requireKeys("balances[0]", b, "product_id", "warehouse_id", "on_hand_qty", "reserved_qty", "available_qty", "version")
	if _, isStr := b["on_hand_qty"].(string); !isStr {
		t.Fatalf("on_hand_qty 应是十进制字符串，实际 %T", b["on_hand_qty"])
	}

	// 业务错误由 handler 交给 c.Error，SDK 引擎里的错误映射中间件再翻成 HTTP
	// 状态码（InvalidArgument → 400）；这里的裸 engine 没有那层中间件，所以直接
	// 核对 handler 交出去的错误码。
	if got := handlerErrorCode(t, "/balances/list", listBalancesHandler(svc), sub, url.Values{"cursor": {"!!!"}}); got != codes.InvalidArgument {
		t.Fatalf("GET /balances/list 的非法游标应交出 InvalidArgument（HTTP 400），实际 %v", got)
	}

	code, raw = serve(t, "/stats/summary", statsSummaryHandler(svc), sub, nil)
	m = decode("GET /stats/summary", code, raw)
	requireKeys("GET /stats/summary", m, "sku_count", "total_on_hand_qty", "low_stock", "low_stock_count")
	if _, isStr := m["total_on_hand_qty"].(string); !isStr {
		t.Fatalf("total_on_hand_qty 应是十进制字符串，实际 %T", m["total_on_hand_qty"])
	}
	if _, isList := m["low_stock"].([]any); !isList {
		t.Fatalf("low_stock 应是数组（没有低库存时是空数组，不是 null），实际 %T", m["low_stock"])
	}
}
