package masterdata

import (
	"context"
	"encoding/json"

	core "github.com/brickKit/erp-inventory/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/calls"
	"github.com/brickKit/be-sdk-go/v3/errs"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
)

// legalEntity is what this component needs of a legal entity.
type legalEntity struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Timezone string `json:"timezone"`
}

// LegalEntityOf asks mdm/org now, outside any transaction (protocol 07: unreachable is not "does not exist").
func LegalEntityOf(m *core.Base, ctx context.Context, id string) (legalEntity, error) {
	conn, ok := m.RT.GRPC(core.Org)
	if !ok {
		return legalEntity{}, errs.New(errs.ReasonDependencyUnavailable, "mdm/org is not reachable: the legal entity cannot be checked")
	}
	res, err := orgv3.NewSourceServiceClient(conn).Read(ctx, &orgv3.ReadRequest{
		Aggregate: orgv3.Aggregate_AGGREGATE_LEGAL_ENTITY, Ids: []string{id}}, calls.Idempotent())
	if err != nil {
		return legalEntity{}, err
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != orgv3.ReadState_READ_STATE_EXISTS {
		return legalEntity{}, core.ErrLegalEntity.New("no legal entity " + id)
	}
	var le legalEntity
	if err := json.Unmarshal(res.GetResults()[0].GetData(), &le); err != nil {
		return le, err
	}
	if le.State != "active" {
		return le, core.ErrLegalEntity.New("the legal entity is " + le.State)
	}
	return le, nil
}

// Product is what this component needs of a product.
type Product struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	BaseUnit string `json:"base_unit"`
	Tracking string `json:"tracking"`
	State    string `json:"state"`
}

// ProductsOf reads the products from mdm/product now, outside any transaction; a product it does not know is absent.
func ProductsOf(m *core.Base, ctx context.Context, ids []string) (map[string]Product, error) {
	conn, ok := m.RT.GRPC(core.Product)
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
	out := map[string]Product{}
	for _, r := range res.GetResults() {
		if r.GetState() != productv3.ReadState_READ_STATE_EXISTS {
			continue
		}
		var p Product
		if err := json.Unmarshal(r.GetData(), &p); err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	return out, nil
}
