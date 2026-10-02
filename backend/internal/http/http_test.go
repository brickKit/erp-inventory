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
