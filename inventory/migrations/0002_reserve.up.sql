-- The branches' state moves to the SDK's participant runtime (be-protocol chapter 16, migration 0024): the state,
-- reason, content digest, hold period, confirmation time, version and times are be_reserve_branch's; this table keeps
-- what a branch takes (the legal entity, the reference, the shortages of a refused hold; the lines in branch_line). A
-- tombstone has no row here. The consumptions' idempotency is be_reserve_consumption's; consumption keeps the
-- movements an issue made.
INSERT INTO be_reserve_branch (id, kind, state, reason, content_digest, hold_expires_at, confirmed_at, version, created_at,
    updated_at)
SELECT id, 'erp.inventory.order', state, reason, content_digest, hold_expires_at, confirmed_at, version, created_at,
    updated_at FROM branch;
INSERT INTO be_reserve_consumption (id, branch_id, content_digest, created_at)
SELECT id, branch_id, content_digest, created_at FROM consumption;
DROP INDEX branch_held;
DELETE FROM branch b WHERE b.state = 'cancelled' AND b.legal_entity_id IS NULL
    AND NOT EXISTS (SELECT 1 FROM branch_line l WHERE l.branch_id = b.id);
ALTER TABLE branch DROP COLUMN state, DROP COLUMN reason, DROP COLUMN content_digest, DROP COLUMN hold_expires_at,
    DROP COLUMN confirmed_at, DROP COLUMN version, DROP COLUMN updated_at;
ALTER TABLE branch ADD CONSTRAINT branch_reserve FOREIGN KEY (id) REFERENCES be_reserve_branch (id);
