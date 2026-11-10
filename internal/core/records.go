package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/jackc/pgx/v5"
)

// Warehouse is a warehouse's complete state (erp.warehouse.changed.v1).
type Warehouse struct {
	ID            string `json:"id"`
	LegalEntityID string `json:"legal_entity_id"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	State         string `json:"state"`
	Version       int64  `json:"version"`
	UpdatedAt     string `json:"updated_at"`
}

// Position is the stock of one product in one warehouse (erp.stock_position.changed.v1).
type Position struct {
	ID            string `json:"id"`
	ProductID     string `json:"product_id"`
	WarehouseID   string `json:"warehouse_id"`
	LegalEntityID string `json:"legal_entity_id"`
	OnHand        string `json:"on_hand"`
	Reserved      string `json:"reserved"`
	Available     string `json:"available"`
	Version       int64  `json:"version"`
	UpdatedAt     string `json:"updated_at"`
}

// Reference names a coordinator's document (and line) on a branch or movement.
type Reference struct {
	Kind     string  `json:"kind"`
	ID       string  `json:"id"`
	Revision *int64  `json:"revision,omitempty"`
	LineID   *string `json:"line_id,omitempty"`
}

// Seq is one sequence number with its epoch (protocol 14).
type Seq struct {
	Epoch string `json:"epoch"`
	Seq   int64  `json:"seq"`
}

// Movement is a stock movement (erp.inventory_movement.recorded.v1); the sequences are nil until numbered.
type Movement struct {
	FactID        string     `json:"fact_id"`
	Kind          string     `json:"kind"`
	ProductID     string     `json:"product_id"`
	WarehouseID   string     `json:"warehouse_id"`
	LegalEntityID string     `json:"legal_entity_id"`
	Quantity      string     `json:"quantity"`
	Batch         *string    `json:"batch"`
	Serials       []string   `json:"serials"`
	BusinessDate  string     `json:"business_date"`
	Reference     *Reference `json:"reference"`
	BranchID      *string    `json:"branch_id"`
	ConsumptionID *string    `json:"consumption_id"`
	// OriginMovementID is a purchase return's receipt movement (3.0.37); omitted otherwise
	OriginMovementID *string `json:"origin_movement_id,omitempty"`
	InventorySeq     *Seq    `json:"inventory_seq"`
	ValuationSeq     *Seq    `json:"valuation_seq"`
	RecordedAt       string  `json:"recorded_at"`
}

// BranchLine is one product and warehouse of a branch.
type BranchLine struct {
	ProductID   string `json:"product_id"`
	WarehouseID string `json:"warehouse_id"`
	Quantity    string `json:"quantity"`
	Consumed    string `json:"consumed"`
}

// Shortage is a line a hold could not reserve.
type Shortage struct {
	ProductID   string `json:"product_id"`
	WarehouseID string `json:"warehouse_id"`
	Requested   string `json:"requested"`
	Available   string `json:"available"`
	Reason      string `json:"reason"`
}

// Branch is a reservation branch (erp.inventory_branch.changed.v1); shortages and the digest stay inside.
type Branch struct {
	ID            string       `json:"id"`
	LegalEntityID *string      `json:"legal_entity_id"`
	Reference     *Reference   `json:"reference"`
	State         string       `json:"state"`
	Reason        *string      `json:"reason"`
	Lines         []BranchLine `json:"lines"`
	HoldExpiresAt *string      `json:"hold_expires_at"`
	Version       int64        `json:"version"`
	UpdatedAt     string       `json:"updated_at"`

	Shortages []Shortage `json:"-"`
	Digest    *string    `json:"-"`
}

const (
	WarehouseCols = `w.id::text, w.legal_entity_id::text, w.code, w.name, w.state, w.version, w.updated_at`
	PositionCols  = `s.id::text, s.product_id::text, s.warehouse_id::text, s.legal_entity_id::text, trim_scale(s.on_hand)::text,
		trim_scale(s.reserved)::text, trim_scale(s.on_hand - s.reserved)::text, s.version, s.updated_at`
	MovementCols = `m.id::text, m.kind, m.product_id::text, m.warehouse_id::text, m.legal_entity_id::text,
		trim_scale(m.quantity)::text, m.batch, m.serials, m.business_date::text, m.ref_kind, m.ref_id, m.ref_line_id,
		m.branch_id::text, m.consumption_id, m.inventory_epoch, m.inventory_seq, m.valuation_epoch, m.valuation_seq,
		m.recorded_at, m.origin_movement_id::text`
	// a branch is the SDK's participant record (be_reserve_branch: state, reason, digest, hold period, version, times)
	// with what it takes here (branch and its lines; none on a tombstone)
	branchCols = `r.id::text, b.legal_entity_id::text, b.ref_kind, b.ref_id, b.ref_revision, r.state, r.reason,
		COALESCE(b.shortages, '[]'::jsonb), r.content_digest, r.hold_expires_at, r.version, r.updated_at,
		COALESCE((SELECT jsonb_agg(jsonb_build_object('product_id', l.product_id::text, 'warehouse_id', l.warehouse_id::text,
			'quantity', trim_scale(l.quantity)::text, 'consumed', trim_scale(l.consumed)::text)
			ORDER BY l.product_id, l.warehouse_id) FROM branch_line l WHERE l.branch_id = r.id), '[]'::jsonb)`
)

func ScanWarehouse(row pgx.Row) (Warehouse, error) {
	var w Warehouse
	var at time.Time
	err := row.Scan(&w.ID, &w.LegalEntityID, &w.Code, &w.Name, &w.State, &w.Version, &at)
	w.UpdatedAt = canon.Time(at)
	return w, err
}

func ScanPosition(row pgx.Row) (Position, error) {
	var p Position
	var at time.Time
	err := row.Scan(&p.ID, &p.ProductID, &p.WarehouseID, &p.LegalEntityID, &p.OnHand, &p.Reserved, &p.Available,
		&p.Version, &at)
	p.UpdatedAt = canon.Time(at)
	return p, err
}

func ScanMovement(row pgx.Row) (Movement, error) {
	var m Movement
	var refKind, refID, refLine, invEpoch, valEpoch *string
	var invSeq, valSeq *int64
	var at time.Time
	err := row.Scan(&m.FactID, &m.Kind, &m.ProductID, &m.WarehouseID, &m.LegalEntityID, &m.Quantity, &m.Batch,
		&m.Serials, &m.BusinessDate, &refKind, &refID, &refLine, &m.BranchID, &m.ConsumptionID, &invEpoch, &invSeq,
		&valEpoch, &valSeq, &at, &m.OriginMovementID)
	if err != nil {
		return m, err
	}
	if m.Serials == nil {
		m.Serials = []string{}
	}
	if refKind != nil {
		m.Reference = &Reference{Kind: *refKind, ID: *refID, LineID: refLine}
	}
	if invSeq != nil {
		m.InventorySeq = &Seq{Epoch: *invEpoch, Seq: *invSeq}
	}
	if valSeq != nil {
		m.ValuationSeq = &Seq{Epoch: *valEpoch, Seq: *valSeq}
	}
	m.RecordedAt = canon.Time(at)
	return m, nil
}

func scanBranch(row pgx.Row) (Branch, error) {
	var b Branch
	var refKind, refID *string
	var refRev *int64
	var shortages, lines []byte
	var expires *time.Time
	var at time.Time
	err := row.Scan(&b.ID, &b.LegalEntityID, &refKind, &refID, &refRev, &b.State, &b.Reason, &shortages, &b.Digest,
		&expires, &b.Version, &at, &lines)
	if err != nil {
		return b, err
	}
	if refKind != nil {
		b.Reference = &Reference{Kind: *refKind, ID: *refID, Revision: refRev}
	}
	if expires != nil && b.State == "held" {
		s := canon.Time(*expires)
		b.HoldExpiresAt = &s
	}
	b.UpdatedAt = canon.Time(at)
	return b, errors.Join(json.Unmarshal(shortages, &b.Shortages), json.Unmarshal(lines, &b.Lines))
}

// errNone is a lookup that found no row.
var ErrNone = errors.New("no such record")

func One[T any](row pgx.Row, scan func(pgx.Row) (T, error)) (T, error) {
	v, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNone
	}
	return v, err
}

func GetWarehouse(ctx context.Context, tx db.Tx, id string, lock bool) (Warehouse, error) {
	q := `SELECT ` + WarehouseCols + ` FROM warehouse w WHERE w.id = $1::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	return One(tx.QueryRow(ctx, q, id), ScanWarehouse)
}

// GetBranch reads a branch; its lock is the SDK's (reserve), taken after the branch's positions.
func GetBranch(ctx context.Context, tx db.Tx, id string) (Branch, error) {
	return One(tx.QueryRow(ctx, `SELECT `+branchCols+` FROM be_reserve_branch r LEFT JOIN branch b ON b.id = r.id
		WHERE r.id = $1::uuid`, id), scanBranch)
}

func GetPosition(ctx context.Context, tx db.Tx, productID, warehouseID string) (Position, error) {
	return One(tx.QueryRow(ctx, `SELECT `+PositionCols+` FROM stock_position s WHERE s.product_id = $1::uuid
		AND s.warehouse_id = $2::uuid`, productID, warehouseID), ScanPosition)
}

// Emit writes a full-state event into the outbox, in the transaction that changed the record (protocol 02, S2).
func Emit(m *Base, ctx context.Context, tx db.Tx, typ, id string, version int64, data any) error {
	_, err := m.RT.Events.Emit(ctx, tx, Stream, events.Event{Type: typ, RecordID: id, Version: version,
		Projection: events.Full, Data: data})
	return err
}
