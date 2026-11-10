//go:build integration

package itest

// The inventory component against PostgreSQL and the bus over its real surfaces (L2). mdm/org and mdm/product are
// stand-ins that speak their published contracts over mTLS; the coordinator is the test, calling as erp/sales. Needs
// BE_SDK_TEST_PG_ADMIN and BE_SDK_TEST_NATS_URL, as be-sdk-go's integration tests. The member listens on 28086 (user)
// and 9096 (system); the stand-ins on 19097 (mdm/org) and 19098 (mdm/product).
// Run: go test -tags integration ./itest/ (make test-integration); the index of the steps is itest/AGENTS.md

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	inventory "github.com/brickKit/erp-inventory/v3/inventory"

	core "github.com/brickKit/erp-inventory/v3/internal/core"
	masterdata "github.com/brickKit/erp-inventory/v3/internal/masterdata"

	"github.com/brickKit/be-sdk-go/v3/config"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	itPort     = "28086"
	itBase     = "http://localhost:" + itPort
	itSystem   = "localhost:9096"
	itOrg      = "localhost:19097"
	itProducts = "localhost:19098"
	itSchema   = "erp_inventory_it"
)

// org is the stand-in mdm/org: every legal entity it knows is active, in Shanghai.
type org struct {
	orgv3.UnimplementedSourceServiceServer
	known map[string]bool
}

func (o *org) Read(_ context.Context, in *orgv3.ReadRequest) (*orgv3.ReadResponse, error) {
	out := &orgv3.ReadResponse{Stamp: &orgv3.Stamp{}}
	for _, id := range in.GetIds() {
		r := &orgv3.ReadResult{Id: id, State: orgv3.ReadState_READ_STATE_PURGED}
		if o.known[id] {
			data, _ := json.Marshal(map[string]any{"id": id, "state": "active", "timezone": "Asia/Shanghai"})
			r = &orgv3.ReadResult{Id: id, State: orgv3.ReadState_READ_STATE_EXISTS, Version: 1, Data: data}
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// products is the stand-in mdm/product.
type products struct {
	productv3.UnimplementedSourceServiceServer
	known map[string]masterdata.Product
}

func (p *products) Read(_ context.Context, in *productv3.ReadRequest) (*productv3.ReadResponse, error) {
	out := &productv3.ReadResponse{Stamp: &productv3.Stamp{}}
	for _, id := range in.GetIds() {
		r := &productv3.ReadResult{Id: id, State: productv3.ReadState_READ_STATE_PURGED}
		if v, ok := p.known[id]; ok {
			data, _ := json.Marshal(v)
			r = &productv3.ReadResult{Id: id, State: productv3.ReadState_READ_STATE_EXISTS, Version: 1, Data: data}
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

type client struct {
	t     *testing.T
	token string
}

func (c client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, itBase+path, r)
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")
	if method == "POST" {
		req.Header.Set("Idempotency-Key", ids.New().String())
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func serve(t *testing.T, addr string, w *sectest.World, as string, register func(*grpc.Server)) {
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(w.TLS(t, as).ServerConfig())))
	register(srv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
}

// fixture is the running member, its stand-ins, the system surface as erp/sales and what one step leaves for the next.
type fixture struct {
	t                     *testing.T
	ctx                   context.Context
	admin                 *pgx.Conn
	w                     *sectest.World
	alice                 client
	res                   inventoryv3.ReservationServiceClient
	stockc                inventoryv3.StockServiceClient
	purchase              inventoryv3.StockServiceClient // as erp/purchase (purchases)
	le, otherLE           string                         // the legal entities
	tea, phone, rice, svc string                         // the products: by batch, by serial, plain, a service
	whSH, whBJ, whHK      string                         // the warehouses: Shanghai and Beijing in le, Hong Kong in otherLE (warehouses)
	sh                    map[string]any                 // the Shanghai warehouse (warehouses)
	order                 string                         // a branch held twice and confirmed, then issued (holdIdempotence)
}

func TestInventoryEndToEnd(t *testing.T) {
	fx := start(t)
	fx.warehouses()
	fx.receipts()
	fx.racingHolds()
	fx.holdIdempotence()
	fx.lateAndExpired()
	fx.issue()
	fx.serialIssue()
	fx.numbering()
	fx.purchases()
	fx.dimensions()
	fx.recoveryChecks()
	fx.participant()
}

// hold holds the lines on a branch in le.
func (fx *fixture) hold(branch string, lines ...*inventoryv3.HoldLine) *inventoryv3.Branch {
	fx.t.Helper()
	h, err := fx.res.Hold(fx.ctx, &inventoryv3.HoldRequest{BranchId: branch, LegalEntityId: fx.le,
		Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-" + branch[:8], Revision: 1}, Lines: lines})
	require.NoError(fx.t, err)
	return h.Branch
}

func line(p, wh, q string) *inventoryv3.HoldLine {
	return &inventoryv3.HoldLine{ProductId: p, WarehouseId: wh, Quantity: q}
}

// position reads the one position of the product in the warehouse.
func (fx *fixture) position(p, wh string) map[string]any {
	_, page := fx.alice.do("GET", "/api/inventory/positions?product_id="+p+"&warehouse_id="+wh, nil)
	ps := page["positions"].([]any)
	require.Len(fx.t, ps, 1)
	return ps[0].(map[string]any)
}

// start makes the world and the stand-ins, migrates and runs the member, and connects as erp/sales.
func start(t *testing.T) *fixture {
	adminURL, natsURL := os.Getenv("BE_SDK_TEST_PG_ADMIN"), os.Getenv("BE_SDK_TEST_NATS_URL")
	if adminURL == "" || natsURL == "" {
		t.Fatal("BE_SDK_TEST_PG_ADMIN and BE_SDK_TEST_NATS_URL are needed")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close(ctx) })
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+itSchema+" CASCADE; CREATE SCHEMA "+itSchema)
	require.NoError(t, err)
	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	pw, _ := u.User.Password()
	w := sectest.New(t)
	w.GrantAll(sectest.Alice, core.Definitions())

	le, otherLE := ids.New().String(), ids.New().String()
	tea, phone, rice, svc := ids.New().String(), ids.New().String(), ids.New().String(), ids.New().String()
	portFree(t, itPort, itSystem, itOrg, itProducts)
	serve(t, itOrg, w, core.Org, func(s *grpc.Server) {
		orgv3.RegisterSourceServiceServer(s, &org{known: map[string]bool{le: true, otherLE: true}})
	})
	serve(t, itProducts, w, core.Product, func(s *grpc.Server) {
		productv3.RegisterSourceServiceServer(s, &products{known: map[string]masterdata.Product{
			tea:   {ID: tea, Kind: "stock", BaseUnit: "C62", Tracking: "batch", State: "active"},
			phone: {ID: phone, Kind: "stock", BaseUnit: "C62", Tracking: "serial", State: "active"},
			rice:  {ID: rice, Kind: "stock", BaseUnit: "KGM", Tracking: "none", State: "active"},
			svc:   {ID: svc, Kind: "service", BaseUnit: "HUR", Tracking: "none", State: "active"}}})
	})

	env := w.Env(t, core.ID)
	for k, v := range map[string]string{"COMPONENT_ID": core.ID, "COMPONENT_VERSION": inventory.Version, "PORT": itPort,
		"STOP_DRAIN_DELAY": "50ms", "PG_HOST": u.Hostname(), "PG_PORT": u.Port(), "PG_DATABASE": strings.TrimPrefix(u.Path, "/"),
		"PG_USER": u.User.Username(), "PG_PASSWORD": pw, "PG_SCHEMA": itSchema, "PG_OWNER_USER": u.User.Username(),
		"PG_OWNER_PASSWORD": pw, "BUS_URL": natsURL, "IMPLEMENTATION_INSTANCE": ids.New().String(),
		"MDM_ORG_GRPC_ENDPOINT": "http://" + itOrg, "MDM_PRODUCT_GRPC_ENDPOINT": "http://" + itProducts} {
		env[k] = v
	}
	var logs bytes.Buffer
	var logMu sync.Mutex
	t.Cleanup(func() {
		if t.Failed() {
			logMu.Lock()
			t.Log(logs.String())
			logMu.Unlock()
		}
	})
	out := &lockedWriter{w: &logs, mu: &logMu}
	opts := launcher.Options{Stdout: out, Stderr: out,
		Env: config.Env{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Read: os.ReadFile}}
	migrate := opts
	migrate.Args = []string{"migrate"}
	require.Equal(t, launcher.ExitOK, launcher.Run(ctx, inventory.Definition(), migrate))
	sctx, stop := context.WithCancel(ctx)
	done := make(chan int, 1)
	go func() { done <- launcher.Run(sctx, inventory.Definition(), opts) }()
	t.Cleanup(func() { stop(); <-done })
	require.Eventually(t, func() bool {
		r, err := http.Get(itBase + "/readyz")
		if err == nil {
			r.Body.Close()
		}
		return err == nil && r.StatusCode == 200
	}, 20*time.Second, 100*time.Millisecond)
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(w.TLS(t, core.Sales).ClientConfig(core.ID))))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	pconn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(w.TLS(t, "erp/purchase").ClientConfig(core.ID))))
	require.NoError(t, err)
	t.Cleanup(func() { pconn.Close() })
	return &fixture{purchase: inventoryv3.NewStockServiceClient(pconn), t: t, ctx: ctx, admin: admin, w: w, alice: client{t, w.Bearer(sectest.Alice)},
		res: inventoryv3.NewReservationServiceClient(conn), stockc: inventoryv3.NewStockServiceClient(conn),
		le: le, otherLE: otherLE, tea: tea, phone: phone, rice: rice, svc: svc}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// portFree fails at once when a port the member needs is taken: a running cell or another member would answer in
// its place, and the test would assert against another process.
func portFree(t *testing.T, addrs ...string) {
	for _, a := range addrs {
		if !strings.Contains(a, ":") {
			a = "localhost:" + a
		}
		ln, err := net.Listen("tcp", a)
		require.NoError(t, err, "port %s is taken: is a cell or another member running?", a)
		_ = ln.Close()
	}
}

// plus adds n to a decimal quantity as the positions answer it (whole numbers in these tests).
func plus(q any, n int) string {
	var v int
	_, _ = fmt.Sscan(q.(string), &v)
	return fmt.Sprint(v + n)
}
