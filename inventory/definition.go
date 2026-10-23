// Package inventory is the inventory component: warehouses, stock movements, positions and reservation branches
// (protocol 04). It is the only writer of physical stock movements.
package inventory

import (
	"embed"
	"io/fs"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/component"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/runtime"
)

// Identity.
const (
	ID       = "erp/inventory"
	Version  = "3.0.2"
	GRPCPort = 9096
)

// Dependencies (the IDs runtime.GRPC takes).
const (
	Product = "mdm/product"
	Org     = "mdm/org"
	// Sales is the coordinator allowed to reserve and issue (protocol 04).
	Sales = "erp/sales"
)

// The stream and its event types (contract/events).
const (
	Stream           = "erp.inventory"
	WarehouseChanged = "erp.warehouse.changed.v1"
	PositionChanged  = "erp.stock_position.changed.v1"
	BranchChanged    = "erp.inventory_branch.changed.v1"
	MovementRecorded = "erp.inventory_movement.recorded.v1"
)

// Resources and operations (contract/openapi.yaml names each route's operation).
const (
	Warehouses     = "erp.inventory.warehouse"
	OpWhRead       = "erp.inventory.warehouse.read"
	OpWhManage     = "erp.inventory.warehouse.manage"
	Stock          = "erp.inventory.stock"
	OpStockRead    = "erp.inventory.stock.read"
	Movements      = "erp.inventory.movement"
	OpMoveRead     = "erp.inventory.movement.read"
	OpMoveCreate   = "erp.inventory.movement.create"
	OpAdjustCreate = "erp.inventory.adjustment.create"
	System         = "erp.inventory.system"
	OpSysRead      = "erp.inventory.system.read"
	OpSysReserve   = "erp.inventory.system.reserve"
	OpSysIssue     = "erp.inventory.system.issue"
)

// Dimensions of the user-surface resources (security 07): the legal entity and the warehouse.
const (
	DimLegalEntity = "legal_entity"
	DimWarehouse   = "warehouse"
)

// Limits and parameters.
const (
	MaxBatch = 500             // pairs a Positions call takes, lines a request carries
	HoldTTL  = 5 * time.Minute // protocol 04's starting value for the hold period
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrations() fs.FS {
	sub, _ := fs.Sub(migrationFiles, "migrations")
	return sub
}

func fields(names ...string) map[string]sdkauthz.Field {
	out := map[string]sdkauthz.Field{}
	for _, n := range names {
		out[n] = sdkauthz.Field{Class: sdkauthz.Ordinary}
	}
	return out
}

// dims are the two value-set dimensions, read from the given warehouse column.
func dims(warehouseColumn string) map[string]sdkauthz.Dimension {
	return map[string]sdkauthz.Dimension{
		DimLegalEntity: {Kind: sdkauthz.ValueSet, Column: "legal_entity_id"},
		DimWarehouse:   {Kind: sdkauthz.ValueSet, Column: warehouseColumn},
	}
}

// Definitions are the component's resources and operations. Warehouses, stock and movements carry the legal entity
// and the warehouse ("a storekeeper sees one warehouse"); the system surface has none.
func Definitions() *sdkauthz.Definitions {
	return &sdkauthz.Definitions{Component: ID,
		Resources: []sdkauthz.Resource{
			{Type: Warehouses, ViewOperation: OpWhRead, Dimensions: dims("id"),
				Fields: fields("id", "legal_entity_id", "code", "name", "state", "version", "updated_at")},
			{Type: Stock, ViewOperation: OpStockRead, Dimensions: dims("warehouse_id"),
				// positions, and the batches and serial numbers listed under the same authorization
				Fields: fields("id", "product_id", "warehouse_id", "legal_entity_id", "on_hand", "reserved", "available",
					"version", "updated_at", "batch", "first_received_at", "serial", "in_stock")},
			{Type: Movements, ViewOperation: OpMoveRead, Dimensions: dims("warehouse_id"),
				Fields: fields("fact_id", "kind", "product_id", "warehouse_id", "legal_entity_id", "quantity", "batch",
					"serials", "business_date", "reference", "branch_id", "consumption_id", "inventory_seq",
					"valuation_seq", "recorded_at")},
			{Type: System, ViewOperation: OpSysRead, Dimensions: map[string]sdkauthz.Dimension{},
				Fields: map[string]sdkauthz.Field{}},
		},
		Operations: []sdkauthz.Operation{
			{Key: OpWhRead, ResourceType: Warehouses, Purpose: "read", RiskClass: "low"},
			{Key: OpWhManage, ResourceType: Warehouses, Purpose: "manage", RiskClass: "low", SideEffects: []string{"business_write"}, Audit: true},
			{Key: OpStockRead, ResourceType: Stock, Purpose: "read", RiskClass: "low"},
			{Key: OpMoveRead, ResourceType: Movements, Purpose: "read", RiskClass: "low"},
			{Key: OpMoveCreate, ResourceType: Movements, Purpose: "create", RiskClass: "low", SideEffects: []string{"business_write"}, Audit: true},
			{Key: OpAdjustCreate, ResourceType: Movements, Purpose: "create", RiskClass: "high", SideEffects: []string{"business_write"}, Audit: true},
			{Key: OpSysRead, ResourceType: System, Purpose: "read", RiskClass: "low"},
			{Key: OpSysReserve, ResourceType: System, Purpose: "action", RiskClass: "low", SideEffects: []string{"business_write"}},
			{Key: OpSysIssue, ResourceType: System, Purpose: "action", RiskClass: "low", SideEffects: []string{"business_write"}, Audit: true},
		}}
}

// Definition is the component as the launcher and the shells take it.
func Definition() component.Definition {
	return component.Definition{ID: ID, Version: Version, Migrations: migrations(), GRPCPort: GRPCPort,
		Streams: []events.Stream{{Name: Stream, Partitions: 4}}, Authz: Definitions(),
		New: func(rt *runtime.Runtime) (component.Module, error) { return New(rt) }}
}
