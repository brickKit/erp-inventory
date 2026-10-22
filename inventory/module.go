package inventory

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/brickKit/be-sdk-go/v3/audit"
	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/calls"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/identity"
	"github.com/brickKit/be-sdk-go/v3/money"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
)

// The component's own reasons (the SDK's catalog has the shared ones).
var (
	errVersion = errs.Define(ID, "VERSION_CONFLICT", errs.Aborted, 409, errs.RetryNo, "The record changed since that version")
	errCode    = errs.Define(ID, "CODE_TAKEN", errs.AlreadyExists, 409, errs.RetryNo, "The code belongs to another warehouse")
	errState   = errs.Define(ID, "STATE_CONFLICT", errs.FailedPrecondition, 409, errs.RetryNo,
		"The record's state does not allow this")
	errLegalEntity = errs.Define(ID, "LEGAL_ENTITY_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The legal entity does not exist or is not active")
	errWarehouse = errs.Define(ID, "WAREHOUSE_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The warehouse does not exist or is not active")
	errProduct = errs.Define(ID, "PRODUCT_NOT_STOCKED", errs.FailedPrecondition, 409, errs.RetryNo,
		"The product does not exist or is not a stock product")
	errTracking = errs.Define(ID, "TRACKING_MISMATCH", errs.FailedPrecondition, 409, errs.RetryNo,
		"The batch or serial numbers do not fit the product's tracking policy")
	errContent = errs.Define(ID, "CONTENT_CONFLICT", errs.AlreadyExists, 409, errs.RetryNo,
		"The ID was used before with other content")
)

var uuidForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Module is the inventory component.
type Module struct {
	inventoryv3.UnimplementedReservationServiceServer
	inventoryv3.UnimplementedStockServiceServer
	rt *runtime.Runtime
}

// New builds the module from its runtime.
func New(rt *runtime.Runtime) (*Module, error) { return &Module{rt: rt}, nil }

// Routes registers the user surface (contract/openapi.yaml).
func (m *Module) Routes(r *httpx.Routes) error {
	add := func(method, path, op string, h httpx.Handler) {
		r.Add(httpx.Route{Method: method, Path: path, Operation: op, Handle: h})
	}
	add("GET", "/api/inventory/warehouses", OpWhRead, m.listWarehouses)
	add("POST", "/api/inventory/warehouses", OpWhManage, m.createWarehouse)
	add("GET", "/api/inventory/warehouses/:id", OpWhRead, m.getWarehouse)
	add("PATCH", "/api/inventory/warehouses/:id", OpWhManage, m.updateWarehouse)
	add("POST", "/api/inventory/receipts", OpMoveCreate, m.receive)
	add("GET", "/api/inventory/movements", OpMoveRead, m.listMovements)
	add("GET", "/api/inventory/movements/:id", OpMoveRead, m.getMovement)
	add("GET", "/api/inventory/positions", OpStockRead, m.listPositions)
	add("GET", "/api/inventory/lots", OpStockRead, m.listLots)
	add("GET", "/api/inventory/serials", OpStockRead, m.listSerials)
	add("GET", "/api/inventory/branches/:id", OpStockRead, m.getBranchHTTP)
	// in the contract, not in this version (D75)
	add("POST", "/api/inventory/transfers", OpMoveCreate, unsupported)
	add("POST", "/api/inventory/adjustments", OpAdjustCreate, unsupported)
	return nil
}

func unsupported(c *httpx.Context) error {
	s, _ := sdkauthz.From(c.Ctx())
	if _, _, err := s.Check(c.Ctx(), c.Operation(), sdkauthz.Facts{DimLegalEntity: sdkauthz.Unobtained,
		DimWarehouse: sdkauthz.Unobtained}); err != nil && !errs.Is(err, errs.PlatformDomain, errs.ReasonAuthorizationUnprovable) {
		return err
	}
	return errs.New(errs.ReasonCapabilityUnsupported, "not in this version of erp/inventory (transfers and adjustments come later)")
}

// Services registers the system surface: only the coordinator reserves and issues (protocol 04).
func (m *Module) Services(s *grpcx.Services) error {
	s.Register(&inventoryv3.ReservationService_ServiceDesc, m, map[string]grpcx.Method{
		"Hold":    {Operation: OpSysReserve, Callers: []string{Sales}},
		"Confirm": {Operation: OpSysReserve, Callers: []string{Sales}},
		"Cancel":  {Operation: OpSysReserve, Callers: []string{Sales}},
		"Query":   {Operation: OpSysReserve, Callers: []string{Sales}},
		"Adjust":  {Operation: OpSysReserve, Callers: []string{Sales}}})
	s.Register(&inventoryv3.StockService_ServiceDesc, m, map[string]grpcx.Method{
		"Issue":     {Operation: OpSysIssue, Callers: []string{Sales}},
		"Positions": {Operation: OpSysRead, Callers: []string{grpcx.AnyComponent}}})
	return nil
}

// scope is the facts of one record's dimensions.
func scope(legalEntity, warehouse string) sdkauthz.Facts {
	return sdkauthz.Facts{DimLegalEntity: sdkauthz.Value(legalEntity), DimWarehouse: sdkauthz.Value(warehouse)}
}

// check decides op on a record in the given scope and returns the context to work in and the acting principal.
func check(c *httpx.Context, op string, facts sdkauthz.Facts) (context.Context, string, error) {
	s, _ := sdkauthz.From(c.Ctx())
	ctx, _, err := s.Check(c.Ctx(), op, facts)
	if err != nil {
		return ctx, "", err
	}
	u, _ := identity.UserFrom(ctx)
	return ctx, u.Subject, nil
}

func invalid(field, why string) error {
	return errs.New(errs.ReasonRequestInvalid, why).WithViolations(errs.Violation{Field: field, Reason: "INVALID", Description: why})
}

func notFound(what string) error { return errs.New(errs.ReasonNotFound, what+" not found") }

// quantity checks a positive decimal string of at most six places (the columns' scale) and returns its canonical form.
func quantity(field, v string) (string, error) {
	d, err := money.Parse(v)
	if err != nil || d.Sign() <= 0 || -d.Exponent > 6 {
		return "", invalid(field, field+" is a positive decimal string with at most 6 places")
	}
	d.Reduce(d)
	return d.Text('f'), nil
}

// change records the business change with the record's new state (protocol 03), in the transaction that made it.
func (m *Module) change(ctx context.Context, tx db.Tx, actor, op, typ, id string, after any) error {
	_, err := m.rt.Audit.Write(ctx, tx, audit.Record{Category: audit.BusinessChange, Operation: op, Outcome: audit.Succeeded,
		Actor: actor, Target: map[string]any{"type": typ, "id": id}, Changes: map[string]any{"after": after}})
	return err
}

// legalEntity is what this component needs of a legal entity.
type legalEntity struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Timezone string `json:"timezone"`
}

// legalEntityOf asks mdm/org now, outside any transaction (protocol 07: unreachable is not "does not exist").
func (m *Module) legalEntityOf(ctx context.Context, id string) (legalEntity, error) {
	conn, ok := m.rt.GRPC(Org)
	if !ok {
		return legalEntity{}, errs.New(errs.ReasonDependencyUnavailable, "mdm/org is not reachable: the legal entity cannot be checked")
	}
	res, err := orgv3.NewSourceServiceClient(conn).Read(ctx, &orgv3.ReadRequest{
		Aggregate: orgv3.Aggregate_AGGREGATE_LEGAL_ENTITY, Ids: []string{id}}, calls.Idempotent())
	if err != nil {
		return legalEntity{}, err
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != orgv3.ReadState_READ_STATE_EXISTS {
		return legalEntity{}, errLegalEntity.New("no legal entity " + id)
	}
	var le legalEntity
	if err := json.Unmarshal(res.GetResults()[0].GetData(), &le); err != nil {
		return le, err
	}
	if le.State != "active" {
		return le, errLegalEntity.New("the legal entity is " + le.State)
	}
	return le, nil
}

// product is what this component needs of a product.
type product struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	BaseUnit string `json:"base_unit"`
	Tracking string `json:"tracking"`
	State    string `json:"state"`
}

// productsOf reads the products from mdm/product now, outside any transaction; a product it does not know is absent.
func (m *Module) productsOf(ctx context.Context, ids []string) (map[string]product, error) {
	conn, ok := m.rt.GRPC(Product)
	if !ok {
		return nil, errs.New(errs.ReasonDependencyUnavailable, "mdm/product is not reachable: the products cannot be checked")
	}
	uniq := map[string]bool{}
	var ask []string
	for _, id := range ids {
		if !uniq[id] {
			uniq[id] = true
			ask = append(ask, id)
		}
	}
	res, err := productv3.NewSourceServiceClient(conn).Read(ctx, &productv3.ReadRequest{
		Aggregate: productv3.Aggregate_AGGREGATE_PRODUCT, Ids: ask}, calls.Idempotent())
	if err != nil {
		return nil, err
	}
	out := map[string]product{}
	for _, r := range res.GetResults() {
		if r.GetState() != productv3.ReadState_READ_STATE_EXISTS {
			continue
		}
		var p product
		if err := json.Unmarshal(r.GetData(), &p); err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	return out, nil
}
