package warehouses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	core "github.com/brickKit/erp-inventory/v3/internal/core"
	masterdata "github.com/brickKit/erp-inventory/v3/internal/masterdata"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
	"github.com/brickKit/be-sdk-go/v3/search"
)

var codeForm = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,31}$`)

// CreateWarehouse creates a warehouse in a legal entity mdm/org says is active now, once per idempotency key.
func CreateWarehouse(m *core.Base, c *httpx.Context) error {
	var in struct {
		LegalEntityID string `json:"legal_entity_id"`
		Code          string `json:"code"`
		Name          string `json:"name"`
	}
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	switch {
	case !core.UUIDForm.MatchString(in.LegalEntityID):
		return core.Invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	case !codeForm.MatchString(in.Code):
		return core.Invalid("code", "code is upper case letters, digits, _ and -, at most 32")
	case strings.TrimSpace(in.Name) == "" || len(in.Name) > 200:
		return core.Invalid("name", "name is 1-200 characters")
	}
	id := ids.New().String()
	ctx, actor, err := core.Check(c, core.OpWhManage, core.Scope(in.LegalEntityID, id))
	if err != nil {
		return err
	}
	if _, err := masterdata.LegalEntityOf(m, ctx, in.LegalEntityID); err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.RT.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		var taken string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM warehouse WHERE legal_entity_id = $1 AND code = $2`,
			in.LegalEntityID, in.Code).Scan(&taken); err == nil {
			return idem.Outcome{}, core.ErrCode.New("warehouse code " + in.Code + " is taken in this legal entity")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO warehouse (id, legal_entity_id, code, name, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, in.LegalEntityID, in.Code, in.Name, search.Text(in.Code, in.Name),
			search.Version); err != nil {
			return idem.Outcome{}, err
		}
		w, err := core.GetWarehouse(ctx, tx, id, false)
		if err != nil {
			return idem.Outcome{}, err
		}
		if err := core.Emit(m, ctx, tx, core.WarehouseChanged, w.ID, w.Version, w); err != nil {
			return idem.Outcome{}, err
		}
		if err := core.Change(m, ctx, tx, actor, core.OpWhManage, core.Warehouses, w.ID, w); err != nil {
			return idem.Outcome{}, err
		}
		res, _ := json.Marshal(map[string]string{"id": w.ID})
		return idem.Outcome{Status: 200, Result: res}, nil
	})
	if err != nil {
		return err
	}
	var res struct{ ID string }
	_ = json.Unmarshal(out.Result, &res)
	w, err := Warehouse(m, ctx, res.ID) // a replay answers with the warehouse as it is now
	if err != nil {
		return err
	}
	return c.JSON(200, w)
}

func Warehouse(m *core.Base, ctx context.Context, id string) (core.Warehouse, error) {
	if !core.UUIDForm.MatchString(id) {
		return core.Warehouse{}, core.NotFound("warehouse")
	}
	var w core.Warehouse
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		w, err = core.GetWarehouse(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, core.ErrNone) {
		return w, core.NotFound("warehouse")
	}
	return w, err
}

func GetWarehouse(m *core.Base, c *httpx.Context) error {
	w, err := Warehouse(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := core.Check(c, core.OpWhRead, core.Scope(w.LegalEntityID, w.ID)); err != nil {
		return err
	}
	return c.JSON(200, w)
}

// UpdateWarehouse renames, deactivates or reactivates; an inactive warehouse takes no new receipt or hold.
func UpdateWarehouse(m *core.Base, c *httpx.Context) error {
	var in struct {
		Name    *string `json:"name"`
		State   *string `json:"state"`
		Version *int64  `json:"version"`
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 200) {
		return core.Invalid("name", "name is 1-200 characters")
	}
	if in.State != nil && *in.State != "active" && *in.State != "inactive" {
		return core.Invalid("state", "state is active or inactive")
	}
	cur, err := Warehouse(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := core.Check(c, core.OpWhManage, core.Scope(cur.LegalEntityID, cur.ID))
	if err != nil {
		return err
	}
	var out core.Warehouse
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		w, err := core.GetWarehouse(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != w.Version {
			return core.ErrVersion.New(fmt.Sprintf("the warehouse is at version %d", w.Version))
		}
		next := w
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.State != nil {
			next.State = *in.State
		}
		if next == w {
			out = w
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE warehouse SET name = $2, state = $3, search = $4, search_version = $5,
			version = version + 1, updated_at = now() WHERE id = $1`, w.ID, next.Name, next.State,
			search.Text(w.Code, next.Name), search.Version); err != nil {
			return err
		}
		if out, err = core.GetWarehouse(ctx, tx, w.ID, false); err != nil {
			return err
		}
		if err := core.Emit(m, ctx, tx, core.WarehouseChanged, out.ID, out.Version, out); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, actor, core.OpWhManage, core.Warehouses, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

// UsableWarehouses locks nothing: it reads the warehouses and fails unless each is active and in the legal entity.
func UsableWarehouses(ctx context.Context, tx db.Tx, legalEntity string, ids []string) (map[string]core.Warehouse, error) {
	out := map[string]core.Warehouse{}
	for _, id := range ids {
		if _, ok := out[id]; ok {
			continue
		}
		w, err := core.GetWarehouse(ctx, tx, id, false)
		if errors.Is(err, core.ErrNone) {
			return nil, core.ErrWarehouse.New("no warehouse " + id)
		}
		if err != nil {
			return nil, err
		}
		if w.State != "active" {
			return nil, core.ErrWarehouse.New("warehouse " + w.Code + " is inactive")
		}
		if legalEntity != "" && w.LegalEntityID != legalEntity {
			return nil, core.ErrWarehouse.New("warehouse " + w.Code + " belongs to another legal entity")
		}
		out[id] = w
	}
	return out, nil
}

// The lists of the user surface (protocol 14: cursor pages; the filters and keyword declared here). Every field is
// ordinary, so a row is listed whole or not at all; the legal entity and warehouse dimensions compile into the WHERE.
var warehouseList = list.Spec{Operation: core.OpWhRead, Alias: "w", IDColumn: "id", IDKind: list.UUID,
	Columns: []list.Column{
		{Field: "code", Column: "code", Kind: list.Text, Sortable: true},
		{Field: "state", Column: "state", Kind: list.Text, Filterable: true},
		{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true}},
	Search: []list.SearchColumn{{Name: "code_name", Column: "search", Fields: []string{"code", "name"}}}}

func ListWarehouses(m *core.Base, c *httpx.Context) error {
	f, err := core.Filters(c, "state", "legal_entity_id")
	if err != nil {
		return err
	}
	ctx, q, err := core.Build(c, warehouseList, f, []list.Order{{Field: "code"}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.RT.DB, q, "warehouse", core.WarehouseCols, core.ScanWarehouse,
		func(w core.Warehouse) ([]string, string) { return []string{w.Code}, w.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"warehouses": out, "next_cursor": next})
}
