DROP TABLE IF EXISTS product_tracking_snapshots;
DROP TABLE IF EXISTS inventory_reservations;
DROP SEQUENCE IF EXISTS inventory_reservation_seq;
DROP TABLE IF EXISTS inventory_movements;   -- CASCADE 到所有月分区
DROP TABLE IF EXISTS inventory_balances;
DROP TABLE IF EXISTS warehouses;
