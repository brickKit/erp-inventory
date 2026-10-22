//go:build integration

package inventory

// The inventory component against PostgreSQL and the bus over its real surfaces (L2). mdm/org and mdm/product are
// stand-ins that speak their published contracts over mTLS; the coordinator is the test, calling as erp/sales. Needs
// BE_SDK_TEST_PG_ADMIN and BE_SDK_TEST_NATS_URL, as be-sdk-go's integration tests. The member listens on 28086 (user)
// and 9096 (system); the stand-ins on 19097 (mdm/org) and 19098 (mdm/product).
// Run: go test -tags integration ./inventory/

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/config"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
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
	known map[string]product
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

func TestInventoryEndToEnd(t *testing.T) {
	adminURL, natsURL := os.Getenv("BE_SDK_TEST_PG_ADMIN"), os.Getenv("BE_SDK_TEST_NATS_URL")
	if adminURL == "" || natsURL == "" {
		t.Fatal("BE_SDK_TEST_PG_ADMIN and BE_SDK_TEST_NATS_URL are needed")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+itSchema+" CASCADE; CREATE SCHEMA "+itSchema)
	require.NoError(t, err)
	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	pw, _ := u.User.Password()
	w := sectest.New(t)
	w.GrantAll(sectest.Alice, Definitions())

	le, otherLE := ids.New().String(), ids.New().String()
	tea, phone, rice, svc := ids.New().String(), ids.New().String(), ids.New().String(), ids.New().String()
	portFree(t, itPort, itSystem, itOrg, itProducts)
	serve(t, itOrg, w, Org, func(s *grpc.Server) {
		orgv3.RegisterSourceServiceServer(s, &org{known: map[string]bool{le: true, otherLE: true}})
	})
	serve(t, itProducts, w, Product, func(s *grpc.Server) {
		productv3.RegisterSourceServiceServer(s, &products{known: map[string]product{
			tea:   {ID: tea, Kind: "stock", BaseUnit: "C62", Tracking: "batch", State: "active"},
			phone: {ID: phone, Kind: "stock", BaseUnit: "C62", Tracking: "serial", State: "active"},
			rice:  {ID: rice, Kind: "stock", BaseUnit: "KGM", Tracking: "none", State: "active"},
			svc:   {ID: svc, Kind: "service", BaseUnit: "HUR", Tracking: "none", State: "active"}}})
	})

	env := w.Env(t, ID)
	for k, v := range map[string]string{"COMPONENT_ID": ID, "COMPONENT_VERSION": Version, "PORT": itPort,
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
	require.Equal(t, launcher.ExitOK, launcher.Run(ctx, Definition(), migrate))
	sctx, stop := context.WithCancel(ctx)
	done := make(chan int, 1)
	go func() { done <- launcher.Run(sctx, Definition(), opts) }()
	defer func() { stop(); <-done }()
	require.Eventually(t, func() bool {
		r, err := http.Get(itBase + "/readyz")
		if err == nil {
			r.Body.Close()
		}
		return err == nil && r.StatusCode == 200
	}, 20*time.Second, 100*time.Millisecond)
	alice := client{t, w.Bearer(sectest.Alice)}

	// warehouses
	code, sh := alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": le, "code": "SH-01", "name": "上海一号仓"})
	require.Equal(t, 200, code, sh)
	_, bj := alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": le, "code": "BJ-01", "name": "北京一号仓"})
	_, hk := alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": otherLE, "code": "HK-01", "name": "香港仓"})
	whSH, whBJ, whHK := sh["id"].(string), bj["id"].(string), hk["id"].(string)
	code, dup := alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": le, "code": "SH-01", "name": "又一个"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "CODE_TAKEN", dup["reason"])
	code, bad := alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": ids.New().String(), "code": "X", "name": "x"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "LEGAL_ENTITY_NOT_USABLE", bad["reason"])

	// receipts: batches, serial numbers, plain stock; the tracking policy decides what a line must name
	receive := func(wh string, lines ...map[string]any) (int, map[string]any) {
		return alice.do("POST", "/api/inventory/receipts", map[string]any{"warehouse_id": wh, "business_date": "2026-10-20", "lines": lines})
	}
	code, r := receive(whSH, map[string]any{"product_id": tea, "quantity": "4", "batch": "T-OLD"})
	require.Equal(t, 200, code, r)
	code, r = receive(whSH, map[string]any{"product_id": tea, "quantity": "6", "batch": "T-NEW"},
		map[string]any{"product_id": rice, "quantity": "5.000"},
		map[string]any{"product_id": phone, "quantity": "3", "serials": []string{"SN-1", "SN-2", "SN-3"}})
	require.Equal(t, 200, code, r)
	_, _ = receive(whBJ, map[string]any{"product_id": rice, "quantity": "5"})
	code, r = receive(whSH, map[string]any{"product_id": tea, "quantity": "1"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "TRACKING_MISMATCH", r["reason"], "a batch-tracked receipt names its batch")
	code, r = receive(whBJ, map[string]any{"product_id": phone, "quantity": "1", "serials": []string{"SN-2"}})
	assert.Equal(t, 409, code)
	assert.Equal(t, "TRACKING_MISMATCH", r["reason"], "a serial number in stock is not received again")
	code, r = receive(whSH, map[string]any{"product_id": svc, "quantity": "1"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "PRODUCT_NOT_STOCKED", r["reason"])

	position := func(p, wh string) map[string]any {
		_, page := alice.do("GET", "/api/inventory/positions?product_id="+p+"&warehouse_id="+wh, nil)
		ps := page["positions"].([]any)
		require.Len(t, ps, 1)
		return ps[0].(map[string]any)
	}
	assert.Equal(t, "10", position(tea, whSH)["on_hand"])
	assert.Equal(t, "5", position(rice, whSH)["available"])

	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(w.TLS(t, Sales).ClientConfig(ID))))
	require.NoError(t, err)
	defer conn.Close()
	res := inventoryv3.NewReservationServiceClient(conn)
	stock := inventoryv3.NewStockServiceClient(conn)
	hold := func(branch string, lines ...*inventoryv3.HoldLine) *inventoryv3.Branch {
		t.Helper()
		h, err := res.Hold(ctx, &inventoryv3.HoldRequest{BranchId: branch, LegalEntityId: le,
			Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-" + branch[:8], Revision: 1}, Lines: lines})
		require.NoError(t, err)
		return h.Branch
	}
	line := func(p, wh, q string) *inventoryv3.HoldLine {
		return &inventoryv3.HoldLine{ProductId: p, WarehouseId: wh, Quantity: q}
	}

	// RS01: ten orders race for five kilograms of rice in Shanghai, one kilogram each: exactly five are held
	var wg sync.WaitGroup
	var mu sync.Mutex
	states := map[inventoryv3.BranchState]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := hold(ids.New().String(), line(rice, whSH, "1"))
			mu.Lock()
			states[b.State]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, 5, states[inventoryv3.BranchState_BRANCH_STATE_HELD], "no oversell: %v", states)
	assert.Equal(t, 5, states[inventoryv3.BranchState_BRANCH_STATE_REJECTED])
	assert.Equal(t, "0", position(rice, whSH)["available"])

	// RS16: two orders lock the same two positions from opposite ends: no deadlock, each all or nothing
	var b1, b2 *inventoryv3.Branch
	wg.Add(2)
	go func() { defer wg.Done(); b1 = hold(ids.New().String(), line(tea, whSH, "3"), line(rice, whBJ, "3")) }()
	go func() { defer wg.Done(); b2 = hold(ids.New().String(), line(rice, whBJ, "3"), line(tea, whSH, "3")) }()
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
	assert.Equal(t, "7", position(tea, whSH)["available"], "the refused hold reserved nothing, the tea included")

	// RS03: the answer to a hold is lost and the coordinator holds again: one reservation
	order := ids.New().String()
	first := hold(order, line(tea, whSH, "2"))
	again := hold(order, line(tea, whSH, "2"))
	assert.Equal(t, first.Version, again.Version)
	assert.Equal(t, "5", position(tea, whSH)["available"])
	// RS17: the same branch ID with another quantity is refused
	_, err = res.Hold(ctx, &inventoryv3.HoldRequest{BranchId: order, LegalEntityId: le,
		Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-" + order[:8], Revision: 1}, Lines: []*inventoryv3.HoldLine{line(tea, whSH, "1")}})
	assert.Equal(t, codes.AlreadyExists, status.Code(err), "CONTENT_CONFLICT")
	// a warehouse of another legal entity is an invalid line, not stock to take
	other := hold(ids.New().String(), line(tea, whHK, "1"))
	assert.Equal(t, "invalid_line", other.Reason)
	assert.Equal(t, "warehouse_not_in_legal_entity", other.Shortages[0].Reason)

	// RS04: the cancel reaches inventory before the hold: the tombstone refuses the late hold
	late := ids.New().String()
	c, err := res.Cancel(ctx, &inventoryv3.CancelRequest{BranchId: late})
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, c.Branch.State)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CANCELLED, hold(late, line(tea, whSH, "1")).State)
	assert.Equal(t, "5", position(tea, whSH)["available"])
	q, err := res.Query(ctx, &inventoryv3.QueryRequest{BranchId: ids.New().String()})
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_ABSENT, q.Branch.State)

	// an expired hold is not revived by a late confirm
	stale := ids.New().String()
	hold(stale, line(tea, whSH, "1"))
	_, err = admin.Exec(ctx, "UPDATE "+itSchema+".branch SET hold_expires_at = now() - interval '1 second' WHERE id = $1", stale)
	require.NoError(t, err)
	cf, err := res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: stale})
	require.NoError(t, err)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_EXPIRED, cf.Branch.State)
	assert.Equal(t, "5", position(tea, whSH)["available"], "the expired hold gave its stock back")

	// RS19: the issue consumes the committed branch in the same transaction; batches go oldest first
	cf, err = res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: order})
	require.NoError(t, err)
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, cf.Branch.State)
	delivery := ids.New().String()
	issue := &inventoryv3.IssueRequest{ConsumptionId: delivery, BranchId: order, BusinessDate: "2026-10-21",
		Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-1"},
		Lines:     []*inventoryv3.IssueLine{{ProductId: tea, WarehouseId: whSH, Quantity: "2"}}}
	is, err := stock.Issue(ctx, issue)
	require.NoError(t, err)
	require.True(t, is.Issued, is.Problems)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CONSUMED, is.BranchState)
	pos := position(tea, whSH)
	assert.Equal(t, "8", pos["on_hand"])
	assert.Equal(t, "5", pos["available"], "the issue took what was held: available unchanged")
	again2, err := stock.Issue(ctx, issue)
	require.NoError(t, err)
	assert.Equal(t, is.MovementIds, again2.MovementIds, "the same delivery again issues nothing more")
	assert.Equal(t, "8", position(tea, whSH)["on_hand"])
	_, lots := alice.do("GET", "/api/inventory/lots?product_id="+tea+"&warehouse_id="+whSH, nil)
	first0 := lots["lots"].([]any)[0].(map[string]any)
	assert.Equal(t, "T-OLD", first0["batch"])
	assert.Equal(t, "2", first0["on_hand"], "FIFO: the older batch went first")

	// the story: a serial-tracked product is issued only by naming its serial numbers
	sn := ids.New().String()
	hold(sn, line(phone, whSH, "2"))
	_, _ = res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: sn})
	is, err = stock.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: ids.New().String(), BranchId: sn, BusinessDate: "2026-10-21",
		Lines: []*inventoryv3.IssueLine{{ProductId: phone, WarehouseId: whSH, Quantity: "2"}}})
	require.NoError(t, err)
	assert.False(t, is.Issued)
	assert.Equal(t, "serial_required", is.Problems[0].Reason)
	is, err = stock.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: ids.New().String(), BranchId: sn, BusinessDate: "2026-10-21",
		Lines: []*inventoryv3.IssueLine{{ProductId: phone, WarehouseId: whSH, Quantity: "2", Serials: []string{"SN-1", "SN-9"}}}})
	require.NoError(t, err)
	assert.Equal(t, "serial_not_found", is.Problems[0].Reason)
	is, err = stock.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: ids.New().String(), BranchId: sn, BusinessDate: "2026-10-21",
		Lines: []*inventoryv3.IssueLine{{ProductId: phone, WarehouseId: whSH, Quantity: "1", Serials: []string{"SN-3"}}}})
	require.NoError(t, err)
	require.True(t, is.Issued, is.Problems)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, is.BranchState, "one of two issued: the branch goes on")

	// the movement events carry both sequences: numbered after commit, the event written in the valuation numbering
	require.Eventually(t, func() bool {
		var n int
		_ = admin.QueryRow(ctx, "SELECT count(*) FROM "+itSchema+".movement WHERE valuation_seq IS NULL").Scan(&n)
		return n == 0
	}, 10*time.Second, 100*time.Millisecond)
	var seqs []int64
	rows, err := admin.Query(ctx, "SELECT inventory_seq FROM "+itSchema+".movement ORDER BY inventory_seq")
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
	require.NoError(t, admin.QueryRow(ctx, "SELECT payload FROM "+itSchema+".be_outbox WHERE type = $1 ORDER BY ord DESC LIMIT 1",
		MovementRecorded).Scan(&payload))
	var ev Movement
	require.NoError(t, json.Unmarshal(payload, &ev))
	assert.NotNil(t, ev.InventorySeq)
	assert.NotNil(t, ev.ValuationSeq)
	var valTea []int64
	rows, err = admin.Query(ctx, "SELECT valuation_seq FROM "+itSchema+".movement WHERE product_id = $1 ORDER BY valuation_seq", tea)
	require.NoError(t, err)
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		valTea = append(valTea, s)
	}
	rows.Close()
	assert.Equal(t, []int64{1, 2, 3}, valTea, "the valuation sequence counts per product and legal entity")

	// the dimensions: Bob keeps the Beijing warehouse only
	all := []string{"id", "product_id", "warehouse_id", "legal_entity_id", "on_hand", "reserved", "available", "version", "updated_at",
		"batch", "first_received_at", "serial", "in_stock"}
	w.Grant(sectest.Bob, sdkauthz.Segment{ResourceType: Stock, Operation: OpStockRead, ReadFields: all,
		Atoms: map[string]sdkauthz.Atom{DimLegalEntity: {Kind: "all"}, DimWarehouse: {Kind: "in", Values: []string{whBJ}}},
		Valid: sdkauthz.Window{From: "2026-01-01T00:00:00Z"}})
	bob := client{t, w.Bearer(sectest.Bob)}
	var ps []any
	require.Eventually(t, func() bool { // the member takes the new grant with its next authorization view
		_, page := bob.do("GET", "/api/inventory/positions", nil)
		ps, _ = page["positions"].([]any)
		return len(ps) > 0
	}, 30*time.Second, 200*time.Millisecond)
	for _, p := range ps {
		assert.Equal(t, whBJ, p.(map[string]any)["warehouse_id"])
	}
	code, _ = bob.do("GET", "/api/inventory/branches/"+order, nil)
	assert.Equal(t, 404, code, "the branch is in Shanghai: Bob cannot see it")
	code, whs := bob.do("GET", "/api/inventory/warehouses", nil)
	assert.Equal(t, 200, code)
	assert.Empty(t, whs["warehouses"], "no warehouse read was granted: a list shows only what the caller may see")
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
