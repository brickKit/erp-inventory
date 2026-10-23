// Command seed fills a running erp/inventory with the seed data for people (be-assembly-standard's seed and test data
// conventions):
//
//   - access, by the authorization admin: the role maintain (the seed user) and read in a legal entity (CN01:
//     test.sales.cn and test.sales.multi; ES01: test.sales.es), every warehouse of it;
//   - warehouses: CN01 上海一号仓 SH-01 and 北京一号仓 BJ-01, ES01 马德里仓 MAD-01, US01 纽约仓 NYC-01; CN01 广州一号仓
//     GZ-01 inactive (refuses receipts and holds: the negative case);
//   - a receipt into each active warehouse: every active stocked product purchasable in its legal entity; batch products
//     in two batches (an older one first: the issue allocates by receipt order), serial products with five serial
//     numbers each, the others plain; BJ-01 has a third of the products only, and one unit of TEA-00-TIN (a hold of two
//     is refused: insufficient stock).
//
// Warehouse codes and the receipts are the fixed keys: a warehouse is looked up by code before it is made, a warehouse
// that has a receipt is not received into again, so a second run creates nothing. The legal entities and products come
// from mdm/org and mdm/product (their seeds run first: make seed).
//
//	SEED_IAM_URL (http://localhost:18200)    the identity member's development exchange
//	SEED_AUTHZ_URL (http://localhost:18223)  the authorization member's effect point
//	SEED_ADMIN (test.alice)                  the authorization admin (the install claim)
//	SEED_USER (test.bob), SEED_PASSWORD      the user that writes the data; the test accounts' password
//	ERP_INVENTORY_URL (http://localhost:18086), MDM_ORG_URL (http://localhost:18083), MDM_PRODUCT_URL (http://localhost:18082)
package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/seed"
)

const prefix = "「本地测试」"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "seed erp/inventory: "+format+"\n", a...)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

type client struct{ seed.Client }

func (c client) must(method, path string, body any, header ...string) map[string]any {
	out, err := c.Must(method, path, body, header...)
	check(err)
	return out
}

// segment grants an operation in the given legal entities (all when none), every warehouse.
func segment(resource, op string, legalEntities ...string) sdkauthz.Segment {
	s := seed.Segment(resource, op)
	le := sdkauthz.Atom{Kind: "all"}
	if len(legalEntities) > 0 {
		le = sdkauthz.Atom{Kind: "in", Values: legalEntities}
	}
	s.Atoms = map[string]sdkauthz.Atom{"legal_entity": le, "warehouse": {Kind: "all"}}
	return s
}

func reads(legalEntities ...string) []sdkauthz.Segment {
	return []sdkauthz.Segment{segment("erp.inventory.warehouse", "erp.inventory.warehouse.read", legalEntities...),
		segment("erp.inventory.stock", "erp.inventory.stock.read", legalEntities...),
		segment("erp.inventory.movement", "erp.inventory.movement.read", legalEntities...)}
}

// legalEntities are mdm/org's seeded legal entities by code.
func legalEntities(org client) map[string]string {
	out := map[string]string{}
	for _, e := range org.must("GET", "/api/org/legal-entities?limit=200", nil)["legal_entities"].([]any) {
		e := e.(map[string]any)
		out[e["code"].(string)] = e["id"].(string)
	}
	for _, code := range []string{"CN01", "ES01", "US01"} {
		if out[code] == "" {
			fail("mdm/org has no legal entity %s: run its seed first", code)
		}
	}
	return out
}

type product struct {
	ID, Code, Kind, Tracking, State string
	Stocked                         bool
	Availability                    []struct {
		LegalEntityID string `json:"legal_entity_id"`
		Purchasable   bool   `json:"purchasable"`
	}
}

// products are every product of mdm/product, page by page.
func products(c client) []product {
	var out []product
	cursor := ""
	for {
		page := c.must("GET", "/api/product/products?limit=200&cursor="+url.QueryEscape(cursor), nil)
		for _, p := range page["products"].([]any) {
			m := p.(map[string]any)
			pr := product{ID: m["id"].(string), Code: m["code"].(string), Kind: m["kind"].(string),
				Tracking: m["tracking"].(string), State: m["state"].(string), Stocked: m["stocked"] == true}
			for _, a := range m["availability"].([]any) {
				a := a.(map[string]any)
				pr.Availability = append(pr.Availability, struct {
					LegalEntityID string `json:"legal_entity_id"`
					Purchasable   bool   `json:"purchasable"`
				}{a["legal_entity_id"].(string), a["purchasable"] == true})
			}
			out = append(out, pr)
		}
		cursor, _ = page["next_cursor"].(string)
		if cursor == "" {
			return out
		}
	}
}

func (p product) purchasableIn(le string) bool {
	for _, a := range p.Availability {
		if a.LegalEntityID == le && a.Purchasable {
			return true
		}
	}
	return false
}

type warehouse struct {
	le, code, name string
	active         bool
}

func main() {
	base := env("ERP_INVENTORY_URL", "http://localhost:18086")
	if !seed.Ready(base) {
		fail("erp/inventory at %s is not ready", base)
	}
	pw := os.Getenv("SEED_PASSWORD")
	if pw == "" {
		fail("SEED_PASSWORD is not set (the test accounts' password in Casdoor)")
	}
	iam, user, admin := env("SEED_IAM_URL", "http://localhost:18200"), env("SEED_USER", "test.bob"), env("SEED_ADMIN", "test.alice")
	accounts, err := seed.Accounts(iam, pw, admin, user, "test.sales.cn", "test.sales.es", "test.sales.multi")
	check(err)
	t := accounts[user].Token
	org := client{seed.NewClient(env("MDM_ORG_URL", "http://localhost:18083"), t)}
	les := legalEntities(org)
	a, err := seed.NewAuthz(env("SEED_AUTHZ_URL", "http://localhost:18223"), accounts[admin].Token, "erp/inventory")
	check(err)
	_, err = a.GrantRoles([]seed.RoleSpec{
		{Key: "maintain", Name: prefix + "库存维护", To: []string{user}, Segments: append(reads(),
			segment("erp.inventory.warehouse", "erp.inventory.warehouse.manage"),
			segment("erp.inventory.movement", "erp.inventory.movement.create"))},
		{Key: "read-cn01", Name: prefix + "库存只读（CN01）", To: []string{"test.sales.cn", "test.sales.multi"}, Segments: reads(les["CN01"])},
		{Key: "read-es01", Name: prefix + "库存只读（ES01）", To: []string{"test.sales.es"}, Segments: reads(les["ES01"])},
	}, accounts)
	check(err)

	c := client{seed.NewClient(base, t)}
	all := products(client{seed.NewClient(env("MDM_PRODUCT_URL", "http://localhost:18082"), t)})
	whs := []warehouse{{"CN01", "SH-01", "上海一号仓", true}, {"CN01", "BJ-01", "北京一号仓", true},
		{"ES01", "MAD-01", "马德里仓", true}, {"US01", "NYC-01", "纽约仓", true}, {"CN01", "GZ-01", "广州一号仓", false}}
	received, made := 0, 0
	for _, w := range whs {
		id, state, isNew := ensureWarehouse(c, les[w.le], w)
		if isNew {
			made++
		}
		if !w.active {
			if state == "active" {
				c.must("PATCH", "/api/inventory/warehouses/"+id, map[string]any{"state": "inactive"})
			}
			continue
		}
		if len(c.must("GET", "/api/inventory/movements?kind=receipt&limit=1&warehouse_id="+id, nil)["movements"].([]any)) > 0 {
			continue // received before
		}
		lines := receipt(all, les[w.le], w.code)
		c.must("POST", "/api/inventory/receipts", map[string]any{"warehouse_id": id, "business_date": "2026-09-01",
			"note": prefix + "期初入库", "lines": lines}, "Idempotency-Key", "seed:erp/inventory:receipt:"+w.code)
		received += len(lines)
	}
	fmt.Printf("seed erp/inventory: %d warehouses made, %d receipt lines\n", made, received)
}

// ensureWarehouse finds a warehouse by its code in the legal entity, or makes it (isNew). The first write waits for the
// seed user's grant to reach erp/inventory.
func ensureWarehouse(c client, le string, w warehouse) (id, state string, isNew bool) {
	page := c.must("GET", "/api/inventory/warehouses?limit=200&legal_entity_id="+le+"&q="+url.QueryEscape(w.code), nil)
	for _, x := range page["warehouses"].([]any) {
		if x := x.(map[string]any); x["code"] == w.code {
			return x["id"].(string), x["state"].(string), false
		}
	}
	code, out, err := c.UntilGranted(30*time.Second, "POST", "/api/inventory/warehouses",
		map[string]any{"legal_entity_id": le, "code": w.code, "name": prefix + w.name},
		"Idempotency-Key", "seed:erp/inventory:warehouse:"+w.code)
	check(err)
	if code != 200 {
		fail("making warehouse %s: %d %v", w.code, code, out)
	}
	return out["id"].(string), out["state"].(string), true
}

// receipt is one warehouse's receipt lines: every active stocked product purchasable in the legal entity.
func receipt(all []product, le, wh string) []map[string]any {
	var lines []map[string]any
	for i, p := range all {
		if !p.Stocked || p.State != "active" || !p.purchasableIn(le) {
			continue
		}
		if wh == "BJ-01" && i%3 != 0 && p.Code != "TEA-00-TIN" {
			continue // Beijing keeps a third of the products
		}
		qty := 100 + (i%7)*20
		if wh == "BJ-01" && p.Code == "TEA-00-TIN" {
			qty = 1 // a hold of two is refused here
		}
		switch p.Tracking {
		case "batch":
			older, newer := qty/2, qty-qty/2
			if qty == 1 {
				older, newer = 1, 0
			}
			lines = append(lines, map[string]any{"product_id": p.ID, "quantity": fmt.Sprint(older), "batch": "B2026-08-" + p.Code})
			if newer > 0 {
				lines = append(lines, map[string]any{"product_id": p.ID, "quantity": fmt.Sprint(newer), "batch": "B2026-09-" + p.Code})
			}
		case "serial":
			var serials []string
			for n := 1; n <= 5; n++ {
				serials = append(serials, fmt.Sprintf("SN-%s-%s-%04d", strings.ReplaceAll(p.Code, "-", ""), wh, n))
			}
			lines = append(lines, map[string]any{"product_id": p.ID, "quantity": "5", "serials": serials})
		default:
			lines = append(lines, map[string]any{"product_id": p.ID, "quantity": fmt.Sprint(qty)})
		}
	}
	return lines
}
