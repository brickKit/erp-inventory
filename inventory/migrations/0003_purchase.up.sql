-- Purchase receipts and returns (3.0.37, the purchase component through StockService.Receive and Return): a return is
-- a movement of its own kind that names the receipt movement it returns from; each call is kept by its own ID with the
-- digest of its content (the same ID with other content is refused) and what it wrote.
ALTER TABLE movement DROP CONSTRAINT movement_kind_check;
ALTER TABLE movement ADD CONSTRAINT movement_kind_check CHECK (kind IN ('receipt', 'issue', 'purchase_return'));
ALTER TABLE movement ADD COLUMN origin_movement_id uuid REFERENCES movement (id);
ALTER TABLE movement ADD CONSTRAINT movement_return_origin CHECK ((kind = 'purchase_return') = (origin_movement_id IS NOT NULL));
CREATE INDEX movement_by_origin ON movement (origin_movement_id) WHERE origin_movement_id IS NOT NULL;

CREATE TABLE purchase_receipt (
    id           text PRIMARY KEY CHECK (id <> ''),
    digest       text NOT NULL,
    movement_ids uuid[] NOT NULL DEFAULT '{}',
    recorded_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE purchase_return (
    id          text PRIMARY KEY CHECK (id <> ''),
    digest      text NOT NULL,
    movement_id uuid REFERENCES movement (id),
    recorded_at timestamptz NOT NULL DEFAULT now()
);
