-- 阶段三 Task 6：warehouse 维数据权限"谁能访问哪个仓库"的分配表。
--
-- ⚠️ 这两维（warehouse/legal_entity）与 org/owner 两维不同：org/owner
-- 直接读 JWT 的 dept_path/sub（besdk.ScopeOf 纯函数），不需要任何额外
-- 表；但"张三能看哪些仓库"这份分配数据全项目任何地方都没有天然来源
-- （不是 JWT 字段，也不该塞进 JWT——warehouse 是本组件的业务概念，
-- 混进身份令牌会让功能权限/数据权限彻底分开这条判据变模糊，阶段三
-- Task 6 讨论定案：分配数据归数据的宿主组件自己维护，不归 infra-authz）。
--
-- sub 不建外键——infra-authz 是另一个 schema，物理上也不该跨 schema
-- 建约束（铁律二的同一条精神）。
CREATE TABLE warehouse_access (
    sub          TEXT        NOT NULL,
    warehouse_id BIGINT      NOT NULL REFERENCES warehouses (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sub, warehouse_id)
);

-- 按 sub 查"我能看哪些仓库"是热路径（List/Get 每次都要查一次）。
CREATE INDEX warehouse_access_sub ON warehouse_access (sub);
