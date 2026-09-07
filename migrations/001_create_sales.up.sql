-- erp-sales 核心表：订单头/行、价格表、客户摘要副本、对账队列。schema
-- 由迁移工具的 search_path 指定，SQL 里不写限定名。
-- ⚠️ 迁移状态表必须落在本组件 schema 里（§11.2.3）：golang-migrate 的
-- x-migrations-table + search_path，见 backend/cmd/migrate/main.go

-- 订单头。按 created_at 月分区（§11.2.5 分区大表清单在册）。
--
-- ⚠️ 本组件是全系统第一个真的需要 dept_id/dept_path/owner_id 三列的
-- 组件，而且要建在分区表上（设计计划 §1：分区表回头加列的代价比建表时
-- 多两个数量级）。dept_id/dept_path 是**创建时快照**，不是运行时查
-- "这个人现在在哪个部门"。
--
-- order_no 不用独立唯一约束——分区表的唯一约束必须含分区键，而 id 本身
-- 就来自一个跨分区共享的序列（BIGSERIAL 建在分区父表上，所有分区共用
-- 同一个序列对象），order_no 直接由 id 派生（"SO" + id），派生值天然
-- 全局唯一，不需要额外的约束机制（设计计划 §9 第 3 条：允许有缺口，
-- 同 erp-finance 的 entry_no 判据）。
CREATE TABLE sales_orders (
    id              BIGSERIAL,
    order_no        TEXT           NOT NULL DEFAULT '',  -- 建单时用 id 派生后回填
    customer_id     TEXT           NOT NULL,              -- 不透明外键，来自 mdm-customer
    customer_name   TEXT           NOT NULL DEFAULT '',   -- 快照
    status          TEXT           NOT NULL DEFAULT 'DRAFT',
    total_amount    NUMERIC(18,2)  NOT NULL DEFAULT 0,
    currency        TEXT           NOT NULL DEFAULT 'CNY', -- 单币种（同 erp-finance 的判据）
    dept_id         TEXT           NOT NULL DEFAULT '',
    dept_path       TEXT           NOT NULL DEFAULT '',
    owner_id        TEXT           NOT NULL DEFAULT '',
    reservation_id  TEXT           NOT NULL DEFAULT '',   -- erp-inventory 的 Reserve 返回值
    compensation_attempts INT      NOT NULL DEFAULT 0,    -- 补偿失败计数（设计计划 §4.4.4）
    suspended_reason TEXT          NOT NULL DEFAULT '',
    -- §11.2.1 强制字段
    created_at      TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version         BIGINT         NOT NULL DEFAULT 1,
    PRIMARY KEY (id, created_at),
    CONSTRAINT sales_orders_status_valid CHECK (status IN
      ('DRAFT', 'CONFIRMED', 'SHIPPED', 'COMPLETED', 'CANCELLED', 'CLOSED', 'SUSPENDED'))
) PARTITION BY RANGE (created_at);
CREATE INDEX sales_orders_customer ON sales_orders (customer_id, created_at);
CREATE INDEX sales_orders_order_no ON sales_orders (order_no);
-- org/owner 数据权限维（设计书 §14.2.2、assembly.yaml 已声明）
CREATE INDEX sales_orders_dept_path ON sales_orders (dept_path text_pattern_ops);
CREATE INDEX sales_orders_owner ON sales_orders (owner_id);

-- ⚠️ 初始分区覆盖当前月起 3 个月（迁移执行时是 2026-09）。其余分区由
-- 组件内置定时任务自动建（决策 54、§11.5.1）——同 erp-inventory 的月
-- 分区判据（订单是纯日历滚动窗口，"下个月总会到来"，不像会计期间需要
-- 显式"开账"）。分区名格式 "表名_YYYY_MM_01"，必须和维护任务生成的
-- 命名一致（见 erp-inventory 设计计划 §9 第 8 条的教训）。
CREATE TABLE sales_orders_2026_09_01 PARTITION OF sales_orders FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE sales_orders_2026_10_01 PARTITION OF sales_orders FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE sales_orders_2026_11_01 PARTITION OF sales_orders FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
ALTER TABLE sales_orders OWNER TO erp_sales_rw;

-- 订单行。⚠️ 子表跟随主表分区（§11.2.4）：分区键用**订单头的**
-- created_at（列名 order_created_at），不用行自己的创建时间——否则补加
-- 的订单行会落进和订单头不同的分区，主表归档时子表搬不干净。
--
-- 全部快照，不 join（设计计划 §2.2）：产品数据在另一个进程另一个
-- schema 里，物理上 join 不到；且产品改名不该重写历史订单。
CREATE TABLE sales_order_items (
    id               BIGSERIAL,
    order_id         BIGINT         NOT NULL,
    order_created_at TIMESTAMPTZ    NOT NULL,  -- = 订单头的 created_at，分区键
    product_id       TEXT           NOT NULL,   -- 不透明外键，来自 mdm-product
    product_sku      TEXT           NOT NULL DEFAULT '',
    product_name     TEXT           NOT NULL DEFAULT '',
    uom_id           TEXT           NOT NULL DEFAULT '',
    qty              NUMERIC(18,6)  NOT NULL,
    unit_price       NUMERIC(18,2)  NOT NULL DEFAULT 0,
    discount         NUMERIC(9,6)   NOT NULL DEFAULT 0,   -- 0~1 的小数
    tax_rate         NUMERIC(9,6)   NOT NULL DEFAULT 0,
    subtotal         NUMERIC(18,2)  NOT NULL DEFAULT 0,
    -- §11.2.1 强制字段
    created_at       TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version          BIGINT         NOT NULL DEFAULT 1,
    status           TEXT           NOT NULL DEFAULT 'ACTIVE',
    PRIMARY KEY (id, order_created_at),
    CONSTRAINT sales_order_items_qty_positive CHECK (qty > 0),
    CONSTRAINT sales_order_items_discount_range CHECK (discount >= 0 AND discount < 1),
    -- 复合外键：引用父表的 (id, created_at) 复合主键，同时校验
    -- order_created_at 确实等于头表的 created_at（设计计划 §7 的
    -- "子表跟随主表"约束落成一条真的能拦的 FK，不只是注释里说说）。
    CONSTRAINT sales_order_items_order_fkey
      FOREIGN KEY (order_id, order_created_at) REFERENCES sales_orders (id, created_at)
) PARTITION BY RANGE (order_created_at);
CREATE INDEX sales_order_items_order ON sales_order_items (order_id, order_created_at);
CREATE INDEX sales_order_items_product ON sales_order_items (product_id);

CREATE TABLE sales_order_items_2026_09_01 PARTITION OF sales_order_items FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE sales_order_items_2026_10_01 PARTITION OF sales_order_items FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE sales_order_items_2026_11_01 PARTITION OF sales_order_items FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
ALTER TABLE sales_order_items OWNER TO erp_sales_rw;

-- 价格表：有序规则表，第一条命中赢，不做规则叠加（设计计划 §3.2）。
CREATE TABLE pricelists (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT           NOT NULL,
    -- §11.2.1 强制字段
    created_at TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version    BIGINT         NOT NULL DEFAULT 1,
    status     TEXT           NOT NULL DEFAULT 'ACTIVE'
);

-- 适用范围三档（GLOBAL/CATEGORY/PRODUCT，对应 Odoo 的 applied_on）+
-- min_quantity + 生效日期区间。price_limit 是 metasfresh 的一等字段
-- （最低售价），不是公式参数——审计"这单为什么能低于底价"时能直接查
-- 这一列（设计计划 §3.2）。
CREATE TABLE pricelist_items (
    id           BIGSERIAL PRIMARY KEY,
    pricelist_id BIGINT         NOT NULL REFERENCES pricelists (id),
    applied_on   TEXT           NOT NULL,             -- GLOBAL/CATEGORY/PRODUCT
    category_id  TEXT           NOT NULL DEFAULT '',  -- applied_on=CATEGORY 时有值
    product_id   TEXT           NOT NULL DEFAULT '',  -- applied_on=PRODUCT 时有值
    min_quantity NUMERIC(18,6)  NOT NULL DEFAULT 0,
    unit_price   NUMERIC(18,2)  NOT NULL,
    discount     NUMERIC(9,6)   NOT NULL DEFAULT 0,
    price_limit  NUMERIC(18,2)  NOT NULL DEFAULT 0,
    tax_rate     NUMERIC(9,6)   NOT NULL DEFAULT 0,
    date_start   DATE,
    date_end     DATE,
    -- §11.2.1 强制字段
    created_at   TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version      BIGINT         NOT NULL DEFAULT 1,
    status       TEXT           NOT NULL DEFAULT 'ACTIVE',
    CONSTRAINT pricelist_items_applied_on_valid CHECK (applied_on IN ('GLOBAL', 'CATEGORY', 'PRODUCT'))
);
CREATE INDEX pricelist_items_lookup ON pricelist_items (applied_on, product_id, category_id, min_quantity DESC);

-- mdm-customer + erp-finance 的摘要副本：credit_limit 来自
-- mdm.customer.*事件消费；credit_exposure 靠定时对账（设计计划 §9
-- 第 6 条：不消费 finance.voucher.posted.v1，那个事件没有 customer_id）。
CREATE TABLE customer_snapshots (
    customer_id     TEXT           PRIMARY KEY,
    credit_limit    NUMERIC(18,2)  NOT NULL DEFAULT 0,
    credit_exposure NUMERIC(18,2)  NOT NULL DEFAULT 0,
    -- §11.2.1 强制字段
    created_at      TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ    NOT NULL DEFAULT now(),
    version         BIGINT         NOT NULL DEFAULT 1,
    status          TEXT           NOT NULL DEFAULT 'ACTIVE'
);

-- "查询也超时"兜底队列（设计计划 §4.5、§9 第 8 条）：Reserve 超时后调
-- GetReservationStatus 本身也超时，不阻塞用户请求，写一行进来交给定时
-- 对账任务处理。阶段二只把行写进去、做成一张可查的表，真正的对账轮询
-- 留到阶段三。
CREATE TABLE sales_order_reconciliation_queue (
    id             BIGSERIAL PRIMARY KEY,
    order_id       BIGINT      NOT NULL,
    reservation_id TEXT        NOT NULL,
    reason         TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ
);
CREATE INDEX sales_order_reconciliation_queue_pending
  ON sales_order_reconciliation_queue (created_at) WHERE resolved_at IS NULL;
