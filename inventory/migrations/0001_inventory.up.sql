-- Warehouses, stock positions with their batches and serial numbers, movements with their two sequences, and the
-- reservation branches of protocol 04. The availability rules are docs/rules/availability.md.

-- A warehouse belongs to one legal entity (an mdm/org ID, opaque); the code is unique in it.
CREATE TABLE warehouse (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    code            text NOT NULL,
    name            text NOT NULL,
    state           text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'inactive')),
    search          text NOT NULL DEFAULT '',
    search_version  int NOT NULL DEFAULT 0,
    version         bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (legal_entity_id, code)
);
CREATE INDEX warehouse_code ON warehouse (code, id);

-- The authority of availability: every command that changes on_hand or reserved locks this row first, several rows
-- in (product_id, warehouse_id) order (RS16). The check is the invariant: available = on_hand - reserved >= 0.
CREATE TABLE stock_position (
    id              uuid PRIMARY KEY,
    product_id      uuid NOT NULL,
    warehouse_id    uuid NOT NULL REFERENCES warehouse (id),
    legal_entity_id uuid NOT NULL,
    on_hand         numeric(19,6) NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    reserved        numeric(19,6) NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    version         bigint NOT NULL DEFAULT 1,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, warehouse_id),
    CHECK (on_hand >= reserved)
);
CREATE INDEX stock_position_warehouse ON stock_position (warehouse_id, product_id);

-- Batches of batch-tracked products in a warehouse, oldest receipt first (the FIFO allocation order).
CREATE TABLE stock_lot (
    product_id        uuid NOT NULL,
    warehouse_id      uuid NOT NULL REFERENCES warehouse (id),
    legal_entity_id   uuid NOT NULL,
    batch             text NOT NULL,
    on_hand           numeric(19,6) NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    first_received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, warehouse_id, batch)
);
CREATE INDEX stock_lot_fifo ON stock_lot (product_id, warehouse_id, first_received_at, batch) WHERE on_hand > 0;

-- Serial numbers of serial-tracked products: where each is and whether it is in stock; kept after the issue.
CREATE TABLE stock_serial (
    product_id   uuid NOT NULL,
    serial       text NOT NULL,
    warehouse_id uuid NOT NULL REFERENCES warehouse (id),
    legal_entity_id uuid NOT NULL,
    in_stock     boolean NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, serial)
);

-- Movements never change (a correction is a reverse movement). They are written without sequences; the numbering
-- transactions give inventory_seq over the whole inventory and, after it, valuation_seq per product and legal entity,
-- each locking its counter and taking the visible unnumbered rows in insertion order (protocol 02, step 2). The
-- movement event goes to the outbox in the valuation numbering, so it carries both.
CREATE TABLE movement (
    id                     uuid PRIMARY KEY,
    ord                    bigint GENERATED ALWAYS AS IDENTITY,
    kind                   text NOT NULL CHECK (kind IN ('receipt', 'issue')),
    product_id             uuid NOT NULL,
    warehouse_id           uuid NOT NULL REFERENCES warehouse (id),
    legal_entity_id        uuid NOT NULL,
    quantity               numeric(19,6) NOT NULL CHECK (quantity > 0),
    batch                  text,
    serials                text[] NOT NULL DEFAULT '{}',
    business_date          date NOT NULL,
    ref_kind               text,
    ref_id                 text,
    ref_line_id            text,
    branch_id              uuid,
    consumption_id         text,
    inventory_epoch        text,
    inventory_seq          bigint,
    valuation_epoch        text,
    valuation_seq          bigint,
    recorded_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (inventory_epoch, inventory_seq),
    UNIQUE (product_id, legal_entity_id, valuation_epoch, valuation_seq)
);
CREATE INDEX movement_unnumbered ON movement (ord) WHERE inventory_seq IS NULL;
CREATE INDEX movement_unvalued ON movement (product_id, legal_entity_id, ord)
    WHERE inventory_seq IS NOT NULL AND valuation_seq IS NULL;
CREATE INDEX movement_list ON movement (recorded_at DESC, id DESC);
CREATE INDEX movement_product ON movement (product_id, warehouse_id, recorded_at DESC);

-- The counters of the two sequences: scope 'inventory' has one row (key ''); scope 'valuation' one row per
-- "<product>/<legal entity>". A new database epoch restarts the row at 1 (protocol 14).
CREATE TABLE seq_counter (
    scope      text NOT NULL CHECK (scope IN ('inventory', 'valuation')),
    key        text NOT NULL,
    epoch      text,
    last_seq   bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, key)
);

-- Reservation branches (protocol 04): the branch ID is the coordinator's. A tombstone (a cancel before the hold) is
-- a branch row in state 'cancelled' with no lines. content_digest makes "the same ID with other content" a conflict.
CREATE TABLE branch (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid,
    ref_kind        text,
    ref_id          text,
    ref_revision    bigint,
    state           text NOT NULL CHECK (state IN ('held', 'committed', 'consumed', 'released', 'expired', 'rejected', 'cancelled')),
    reason          text,
    shortages       jsonb NOT NULL DEFAULT '[]'::jsonb,
    content_digest  text,
    hold_expires_at timestamptz,
    confirmed_at    timestamptz,
    version         bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX branch_held ON branch (hold_expires_at) WHERE state = 'held';

CREATE TABLE branch_line (
    branch_id    uuid NOT NULL REFERENCES branch (id),
    product_id   uuid NOT NULL,
    warehouse_id uuid NOT NULL,
    quantity     numeric(19,6) NOT NULL CHECK (quantity > 0),
    consumed     numeric(19,6) NOT NULL DEFAULT 0 CHECK (consumed >= 0 AND consumed <= quantity),
    PRIMARY KEY (branch_id, product_id, warehouse_id)
);

-- One issue a consumption ID (the coordinator's delivery): the idempotency of StockService.Issue.
CREATE TABLE consumption (
    id             text PRIMARY KEY,
    branch_id      uuid NOT NULL,
    content_digest text NOT NULL,
    movement_ids   uuid[] NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
