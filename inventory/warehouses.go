package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/search"
)

var codeForm = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,31}$`)

// createWarehouse creates a warehouse in a legal entity mdm/org says is active now, once per idempotency key.
func (m *Module) createWarehouse(c *httpx.Context) error {
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
	case !uuidForm.MatchString(in.LegalEntityID):
		return invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	case !codeForm.MatchString(in.Code):
		return invalid("code", "code is upper case letters, digits, _ and -, at most 32")
	case strings.TrimSpace(in.Name) == "" || len(in.Name) > 200:
		return invalid("name", "name is 1-200 characters")
	}
	id := ids.New().String()
	ctx, actor, err := check(c, OpWhManage, scope(in.LegalEntityID, id))
	if err != nil {
		return err
	}
	if _, err := m.legalEntityOf(ctx, in.LegalEntityID); err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.rt.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		var taken string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM warehouse WHERE legal_entity_id = $1 AND code = $2`,
			in.LegalEntityID, in.Code).Scan(&taken); err == nil {
			return idem.Outcome{}, errCode.New("warehouse code " + in.Code + " is taken in this legal entity")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO warehouse (id, legal_entity_id, code, name, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, in.LegalEntityID, in.Code, in.Name, search.Text(in.Code, in.Name),
			search.Version); err != nil {
			return idem.Outcome{}, err
		}
		w, err := getWarehouse(ctx, tx, id, false)
		if err != nil {
			return idem.Outcome{}, err
		}
		if err := m.emit(ctx, tx, WarehouseChanged, w.ID, w.Version, w); err != nil {
			return idem.Outcome{}, err
		}
		if err := m.change(ctx, tx, actor, OpWhManage, Warehouses, w.ID, w); err != nil {
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
	w, err := m.warehouse(ctx, res.ID) // a replay answers with the warehouse as it is now
	if err != nil {
		return err
	}
	return c.JSON(200, w)
}

func (m *Module) warehouse(ctx context.Context, id string) (Warehouse, error) {
	if !uuidForm.MatchString(id) {
		return Warehouse{}, notFound("warehouse")
	}
	var w Warehouse
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		w, err = getWarehouse(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, errNone) {
		return w, notFound("warehouse")
	}
	return w, err
}

func (m *Module) getWarehouse(c *httpx.Context) error {
	w, err := m.warehouse(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := check(c, OpWhRead, scope(w.LegalEntityID, w.ID)); err != nil {
		return err
	}
	return c.JSON(200, w)
}

// updateWarehouse renames, deactivates or reactivates; an inactive warehouse takes no new receipt or hold.
func (m *Module) updateWarehouse(c *httpx.Context) error {
	var in struct {
		Name    *string `json:"name"`
		State   *string `json:"state"`
		Version *int64  `json:"version"`
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 200) {
		return invalid("name", "name is 1-200 characters")
	}
	if in.State != nil && *in.State != "active" && *in.State != "inactive" {
		return invalid("state", "state is active or inactive")
	}
	cur, err := m.warehouse(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := check(c, OpWhManage, scope(cur.LegalEntityID, cur.ID))
	if err != nil {
		return err
	}
	var out Warehouse
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		w, err := getWarehouse(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != w.Version {
			return errVersion.New(fmt.Sprintf("the warehouse is at version %d", w.Version))
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
		if out, err = getWarehouse(ctx, tx, w.ID, false); err != nil {
			return err
		}
		if err := m.emit(ctx, tx, WarehouseChanged, out.ID, out.Version, out); err != nil {
			return err
		}
		return m.change(ctx, tx, actor, OpWhManage, Warehouses, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

// usableWarehouses locks nothing: it reads the warehouses and fails unless each is active and in the legal entity.
func usableWarehouses(ctx context.Context, tx db.Tx, legalEntity string, ids []string) (map[string]Warehouse, error) {
	out := map[string]Warehouse{}
	for _, id := range ids {
		if _, ok := out[id]; ok {
			continue
		}
		w, err := getWarehouse(ctx, tx, id, false)
		if errors.Is(err, errNone) {
			return nil, errWarehouse.New("no warehouse " + id)
		}
		if err != nil {
			return nil, err
		}
		if w.State != "active" {
			return nil, errWarehouse.New("warehouse " + w.Code + " is inactive")
		}
		if legalEntity != "" && w.LegalEntityID != legalEntity {
			return nil, errWarehouse.New("warehouse " + w.Code + " belongs to another legal entity")
		}
		out[id] = w
	}
	return out, nil
}
