-- erp-inventory 核心表：余额、流水、预留、仓库主数据、产品追踪摘要副本。
-- schema 由迁移工具的 search_path 指定，SQL 里不写限定名。
-- ⚠️ 迁移状态表必须落在本组件 schema 里（§11.2.3）：golang-migrate 的
-- x-migrations-table + search_path，见 backend/cmd/migrate/main.go

-- 仓库/库位主数据，几十行量级，不分区（设计计划 §2、§7）。
CREATE TABLE warehouses (
    id         BIGSERIAL PRIMARY KEY,
    code       TEXT           NOT NULL,
    name       TEXT           NOT NULL,
    -- §11.2.1 强制字段
    created_at TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version    BIGINT         NOT NULL DEFAULT 1,
    status     TEXT           NOT NULL DEFAULT 'ACTIVE'
);
CREATE UNIQUE INDEX warehouses_code_uniq ON warehouses (code);

-- 当前余额。⚠️ §11.2.5 明确注明它必须永远小而快——不分区、不归档
-- （设计计划 §7）。行数上限是「SKU 数 × 仓库数」，是个有界值。
--
-- product_id 是不透明外键（TEXT）：本组件不依赖 mdm-product，校验产品
-- 是否存在是调用方（erp-sales）的事（设计计划 §5）。warehouse_id 是
-- 本 schema 内的真实外键（BIGINT REFERENCES warehouses）。
CREATE TABLE inventory_balances (
    id            BIGSERIAL PRIMARY KEY,
    product_id    TEXT           NOT NULL,
    warehouse_id  BIGINT         NOT NULL REFERENCES warehouses (id),
    on_hand_qty   NUMERIC(18,6)  NOT NULL DEFAULT 0,
    reserved_qty  NUMERIC(18,6)  NOT NULL DEFAULT 0,
    -- §11.2.1 强制字段
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version       BIGINT         NOT NULL DEFAULT 1,
    status        TEXT           NOT NULL DEFAULT 'ACTIVE',
    -- 双重防线：条件更新已经是物理级防超卖（设计计划 §2.2，判定与加锁
    -- 是同一条 UPDATE 语句），这两条 CHECK 是数据库层的兜底——即使应用层
    -- 将来某处手滑绕过了条件更新，数据库也不允许这一行落到非法状态。
    CONSTRAINT inventory_balances_non_negative CHECK (on_hand_qty >= 0 AND reserved_qty >= 0),
    CONSTRAINT inventory_balances_no_oversell  CHECK (reserved_qty <= on_hand_qty)
);
-- ⚠️ 必须有这个唯一约束——查证发现 Odoo 的 stock.quant 没有它，并发下
-- 产生重复行，靠一个有已知 bug 的合并任务事后清理（设计计划 §2.2、§8）。
CREATE UNIQUE INDEX inventory_balances_product_warehouse_uniq
  ON inventory_balances (product_id, warehouse_id);
CREATE INDEX inventory_balances_warehouse ON inventory_balances (warehouse_id);

-- 库存流水：只增不改，永不 UPDATE/DELETE（设计计划 §2.1）。按
-- created_at 月分区——§11.2.5 分区大表清单里交易流水按月，不是
-- outbox/inbox 那种按周。
CREATE TABLE inventory_movements (
    id            BIGSERIAL,
    product_id    TEXT           NOT NULL,
    warehouse_id  BIGINT         NOT NULL REFERENCES warehouses (id),
    qty           NUMERIC(18,6)  NOT NULL,   -- 带符号：出库为负，入库/盘盈为正
    reason        TEXT           NOT NULL,   -- RECEIVE/ISSUE/ADJUST_GAIN/ADJUST_LOSS，判断逻辑用它
    note          TEXT           NOT NULL DEFAULT '',  -- Adjust 请求里那句人类可读的调整原因（如"盘点差异"），纯展示，不参与任何判断
    order_id      TEXT           NOT NULL DEFAULT '',  -- 仅幂等键/追溯字段，不参与任何判断逻辑（设计计划 §1）
    batch_no      TEXT           NOT NULL DEFAULT '',
    serial_no     TEXT           NOT NULL DEFAULT '',
    -- §11.2.1 强制字段。⚠️ 这一行写完这四列就再也不会被 UPDATE——保留
    -- 只是为了和全系统的表结构约定一致（设计计划 §2："全部符合 §11.2.1"）。
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version       BIGINT         NOT NULL DEFAULT 1,
    status        TEXT           NOT NULL DEFAULT 'ACTIVE',
    PRIMARY KEY (id, created_at),
    CONSTRAINT inventory_movements_reason_valid
      CHECK (reason IN ('RECEIVE', 'ISSUE', 'ADJUST_GAIN', 'ADJUST_LOSS')),
    -- 序列号追踪的产品，每一行数量必须是 ±1（设计计划 §9 第 3 条）。
    -- ⚠️ 这条约束看不到 tracking_type（它在摘要副本里，不在这张表上）——
    -- 只能管「填了序列号的行数量是不是 1」，管不了「该填序列号的行没填」，
    -- 后一半的缺口是刻意接受的（设计计划 §9 第 3 条）。
    CONSTRAINT inventory_movements_serial_qty_one CHECK (serial_no = '' OR abs(qty) = 1)
) PARTITION BY RANGE (created_at);
CREATE INDEX inventory_movements_product_warehouse ON inventory_movements (product_id, warehouse_id, created_at);
CREATE INDEX inventory_movements_warehouse ON inventory_movements (warehouse_id, created_at);

-- ⚠️ 初始分区覆盖当前月起 3 个月（迁移执行时是 2026-09）。其余分区由
-- 组件内置定时任务自动建（决策 54、§11.5.1，backend/internal/partition/
-- monthly.go）。⚠️ 分区名必须是 "表名_YYYY_MM_01"（月份的第一天，不是
-- "表名_YYYY_MM"）——它要和 monthly.go 里 ensurePartition 生成的名字
-- 完全一致，否则维护任务的 to_regclass 会查不到这几个已存在的分区，
-- 转而尝试新建同一段时间范围的分区，撞上 PostgreSQL 的分区范围不许
-- 重叠而报错（这是实现 monthly.go 时发现的：两处生成分区名的代码路径
-- 必须共用同一套格式，不能各写各的）。
CREATE TABLE inventory_movements_2026_09_01 PARTITION OF inventory_movements
  FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE inventory_movements_2026_10_01 PARTITION OF inventory_movements
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE inventory_movements_2026_11_01 PARTITION OF inventory_movements
  FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
-- 同 mdm-product 002 迁移的实测踩坑：建分区（CREATE TABLE ... PARTITION OF）
-- 要求执行者是父表 owner。迁移用管理凭据跑，建出来的分区默认属于那个
-- 账号；分区维护后台任务运行时用 erp_inventory_rw（SET LOCAL ROLE 切换）
-- 建未来的分区，两者不是同一身份，必须显式把 owner 转过去。
ALTER TABLE inventory_movements OWNER TO erp_inventory_rw;

-- 未决预留 + 已终结的预留（终态行定期清理，热数据永远小，设计计划 §7）。
-- ⚠️ 一行 = 一次 Reserve 调用里的一个 (product, warehouse) 项。
-- reservation_id 是同一次 Reserve 调用共享的分组键（从下面这个序列取号，
-- 作为 ReserveResponse.reservation_id 返回给调用方）——CancelReservation/
-- ConfirmIssue/GetReservationStatus 按 reservation_id 操作这一组行，
-- 组内所有行的 status 总是在同一个事务里同步迁移
-- （RESERVED → CONFIRMED 或 RESERVED → CANCELLED），不存在组内状态不一致。
CREATE SEQUENCE inventory_reservation_seq;

CREATE TABLE inventory_reservations (
    id             BIGSERIAL PRIMARY KEY,
    reservation_id BIGINT         NOT NULL,
    order_id       TEXT           NOT NULL DEFAULT '',   -- 仅追溯字段，不参与判断逻辑
    product_id     TEXT           NOT NULL,
    warehouse_id   BIGINT         NOT NULL REFERENCES warehouses (id),
    qty            NUMERIC(18,6)  NOT NULL,
    status         TEXT           NOT NULL DEFAULT 'RESERVED',
    -- §11.2.1 强制字段
    created_at     TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version        BIGINT         NOT NULL DEFAULT 1,
    CONSTRAINT inventory_reservations_qty_positive CHECK (qty > 0),
    -- ⚠️ RESERVED/CONFIRMED/CANCELLED 三个是唯一合法值，且 NOT_FOUND
    -- 从来不是一行的取值——它是「reservation_id 查不到任何行」这件事本身
    -- （设计计划 §3、§4.5：GetReservationStatus 必须能区分 NOT_FOUND 与
    -- CANCELLED，不许合并成一个"没有"）。
    CONSTRAINT inventory_reservations_status_valid
      CHECK (status IN ('RESERVED', 'CONFIRMED', 'CANCELLED'))
);
CREATE INDEX inventory_reservations_group ON inventory_reservations (reservation_id);
CREATE INDEX inventory_reservations_warehouse ON inventory_reservations (warehouse_id);
-- 同一次 Reserve 调用里不接受重复的 (product, warehouse) 项——防应用层
-- 手滑传重复行导致同一个余额被计入两次。
CREATE UNIQUE INDEX inventory_reservations_group_item_uniq
  ON inventory_reservations (reservation_id, product_id, warehouse_id);
-- GetReservationStatus 靠 reservation_id 查；status != RESERVED（终态）的行
-- 按设计计划 §7 定期清理，这个索引只覆盖未决的，保持它小。
CREATE INDEX inventory_reservations_pending ON inventory_reservations (reservation_id)
  WHERE status = 'RESERVED';

-- mdm-product 的摘要副本：只取 tracking_type（设计计划 §5）。按 version
-- 单调更新（§3.10）——消费 mdm.product.created.v1/.updated.v1 时维护。
CREATE TABLE product_tracking_snapshots (
    product_id     TEXT        PRIMARY KEY,
    tracking_type  TEXT        NOT NULL DEFAULT 'NONE',
    -- §11.2.1 强制字段
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    version        BIGINT      NOT NULL DEFAULT 1,
    status         TEXT        NOT NULL DEFAULT 'ACTIVE'
);
