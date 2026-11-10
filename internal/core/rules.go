package core

import (
	"context"
	"regexp"

	"github.com/brickKit/be-sdk-go/v3/audit"
	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/identity"
	"github.com/brickKit/be-sdk-go/v3/list"
	"github.com/brickKit/be-sdk-go/v3/money"
)

// The component's own reasons (the SDK's catalog has the shared ones).
var (
	ErrVersion = errs.Define(ID, "VERSION_CONFLICT", errs.Aborted, 409, errs.RetryNo, "The record changed since that version")
	ErrCode    = errs.Define(ID, "CODE_TAKEN", errs.AlreadyExists, 409, errs.RetryNo, "The code belongs to another warehouse")
	errState   = errs.Define(ID, "STATE_CONFLICT", errs.FailedPrecondition, 409, errs.RetryNo,
		"The record's state does not allow this")
	ErrLegalEntity = errs.Define(ID, "LEGAL_ENTITY_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The legal entity does not exist or is not active")
	ErrWarehouse = errs.Define(ID, "WAREHOUSE_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The warehouse does not exist or is not active")
	ErrProduct = errs.Define(ID, "PRODUCT_NOT_STOCKED", errs.FailedPrecondition, 409, errs.RetryNo,
		"The product does not exist or is not a stock product")
	ErrTracking = errs.Define(ID, "TRACKING_MISMATCH", errs.FailedPrecondition, 409, errs.RetryNo,
		"The batch or serial numbers do not fit the product's tracking policy")
	ErrContent = errs.Define(ID, "CONTENT_CONFLICT", errs.AlreadyExists, 409, errs.RetryNo,
		"The ID was used before with other content")
	// ErrReturn: a purchase return that cannot be made from its receipt (not a receipt, more than it has left, a serial
	// number it did not receive, not available now)
	ErrReturn = errs.Define(ID, "RETURN_NOT_POSSIBLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The return cannot be made from that receipt")
)

var UUIDForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func Unsupported(c *httpx.Context) error {
	s, _ := sdkauthz.From(c.Ctx())
	if _, _, err := s.Check(c.Ctx(), c.Operation(), sdkauthz.Facts{DimLegalEntity: sdkauthz.Unobtained,
		DimWarehouse: sdkauthz.Unobtained}); err != nil && !errs.Is(err, errs.PlatformDomain, errs.ReasonAuthorizationUnprovable) {
		return err
	}
	return errs.New(errs.ReasonCapabilityUnsupported, "not in this version of erp/inventory (transfers and adjustments come later)")
}

// Scope is the facts of one record's dimensions.
func Scope(legalEntity, warehouse string) sdkauthz.Facts {
	return sdkauthz.Facts{DimLegalEntity: sdkauthz.Value(legalEntity), DimWarehouse: sdkauthz.Value(warehouse)}
}

// Check decides op on a record in the given scope and returns the context to work in and the acting principal.
func Check(c *httpx.Context, op string, facts sdkauthz.Facts) (context.Context, string, error) {
	s, _ := sdkauthz.From(c.Ctx())
	ctx, _, err := s.Check(c.Ctx(), op, facts)
	if err != nil {
		return ctx, "", err
	}
	u, _ := identity.UserFrom(ctx)
	return ctx, u.Subject, nil
}

func Invalid(field, why string) error {
	return errs.New(errs.ReasonRequestInvalid, why).WithViolations(errs.Violation{Field: field, Reason: "INVALID", Description: why})
}

func NotFound(what string) error { return errs.New(errs.ReasonNotFound, what+" not found") }

// Quantity checks a positive decimal string of at most six places (the columns' scale) and returns its canonical form.
func Quantity(field, v string) (string, error) {
	d, err := money.Parse(v)
	if err != nil || d.Sign() <= 0 || -d.Exponent > 6 {
		return "", Invalid(field, field+" is a positive decimal string with at most 6 places")
	}
	d.Reduce(d)
	return d.Text('f'), nil
}

// Change records the business change with the record's new state (protocol 03), in the transaction that made it.
func Change(m *Base, ctx context.Context, tx db.Tx, actor, op, typ, id string, after any) error {
	_, err := m.RT.Audit.Write(ctx, tx, audit.Record{Category: audit.BusinessChange, Operation: op, Outcome: audit.Succeeded,
		Actor: actor, Target: audit.TargetOf(typ, id, after), Changes: map[string]any{"after": after}})
	return err
}

// Filters turns the given query parameters into equality filters, checking the ID ones.
func Filters(c *httpx.Context, names ...string) ([]list.Filter, error) {
	var f []list.Filter
	for _, k := range names {
		v := c.Query(k)
		if v == "" {
			continue
		}
		if (k == "product_id" || k == "warehouse_id" || k == "legal_entity_id") && !UUIDForm.MatchString(v) {
			return nil, Invalid(k, k+" is an ID")
		}
		f = append(f, list.Filter{Field: k, Op: "eq", Values: []string{v}})
	}
	return f, nil
}

func Build(c *httpx.Context, spec list.Spec, f []list.Filter, sort []list.Order) (context.Context, *list.Query, error) {
	limit, err := list.ParseLimit(c.Query("limit"))
	if err != nil {
		return nil, nil, err
	}
	s, _ := sdkauthz.From(c.Ctx())
	return list.Build(c.Ctx(), s, spec, list.Request{Filters: f, Q: c.Query("q"), Limit: limit, Cursor: c.Query("cursor"),
		Sort: sort})
}
