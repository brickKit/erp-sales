-- Price lists, quotations, orders with their lines and billable ledger, confirmation processes (protocol 04),
-- deliveries, and billing claims (protocol 05). The rules are docs/design.md, docs/rules/pricing.md and docs/flows/.

CREATE TABLE price_list (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    currency        text NOT NULL,
    code            text NOT NULL,
    name            text NOT NULL,
    state           text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'inactive')),
    version         bigint NOT NULL DEFAULT 1,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (legal_entity_id, code)
);

-- A price net of tax for one product and unit, valid from a date; adding a newer one ends the previous the day before.
CREATE TABLE price_list_item (
    id            uuid PRIMARY KEY,
    price_list_id uuid NOT NULL REFERENCES price_list (id),
    product_id    uuid NOT NULL,
    unit          text NOT NULL,
    price         numeric(19,6) NOT NULL CHECK (price >= 0),
    valid_from    date NOT NULL,
    valid_to      date,
    version       bigint NOT NULL DEFAULT 1,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (price_list_id, product_id, unit, valid_from)
);

-- The document columns shared by quotations and orders; department_path is the org-tree fact of security 07, copied
-- from the department projection and rewritten when the department moves.
CREATE TABLE quotation (
    id              uuid PRIMARY KEY,
    number          text NOT NULL UNIQUE,
    legal_entity_id uuid NOT NULL,
    customer_id     uuid NOT NULL,
    customer_name   text NOT NULL,
    customer_tax_id text,
    currency        text NOT NULL,
    valid_until     date,
    owner_id        text NOT NULL,
    department_id   uuid,
    department_path text,
    state           text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'sent', 'accepted', 'cancelled', 'expired')),
    order_id        uuid,
    net             numeric(19,4) NOT NULL DEFAULT 0,
    tax             numeric(19,4) NOT NULL DEFAULT 0,
    search          text NOT NULL DEFAULT '',
    search_version  int NOT NULL DEFAULT 0,
    version         bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX quotation_list ON quotation (created_at DESC, id);
CREATE INDEX quotation_department ON quotation (department_id);

CREATE TABLE quotation_line (
    id                 uuid PRIMARY KEY,
    quotation_id       uuid NOT NULL REFERENCES quotation (id),
    line_no            int NOT NULL,
    kind               text NOT NULL CHECK (kind IN ('goods', 'service')),
    product_id         uuid NOT NULL,
    product_code       text NOT NULL,
    product_name       text NOT NULL,
    base_unit          text NOT NULL,
    quantity           numeric(19,6) NOT NULL CHECK (quantity > 0),
    unit               text NOT NULL,
    conversion_factor  numeric(24,12) NOT NULL,
    base_quantity      numeric(19,6) NOT NULL,
    warehouse_id       uuid,
    unit_price         numeric(19,6) NOT NULL,
    price_source       text NOT NULL CHECK (price_source IN ('price_list', 'manual')),
    price_list_item_id uuid,
    tax_rate           numeric(9,6) NOT NULL,
    net                numeric(19,4) NOT NULL,
    tax                numeric(19,4) NOT NULL,
    UNIQUE (quotation_id, line_no)
);

CREATE TABLE sales_order (
    id                  uuid PRIMARY KEY,
    number              text NOT NULL UNIQUE,
    legal_entity_id     uuid NOT NULL,
    customer_id         uuid NOT NULL,
    customer_name       text NOT NULL,
    customer_tax_id     text,
    currency            text NOT NULL,
    business_date       date NOT NULL,
    rate                numeric(19,10),
    rate_date           date,
    rate_type           text,
    rate_record_id      uuid,
    rate_record_version bigint,
    owner_id            text NOT NULL,
    department_id       uuid,
    department_path     text,
    state               text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'confirming', 'confirmed', 'completed', 'cancelled')),
    revision            int NOT NULL DEFAULT 0,
    quotation_id        uuid,
    confirmation_id     uuid,
    reason              text,
    net                 numeric(19,4) NOT NULL DEFAULT 0,
    tax                 numeric(19,4) NOT NULL DEFAULT 0,
    search              text NOT NULL DEFAULT '',
    search_version      int NOT NULL DEFAULT 0,
    version             bigint NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sales_order_list ON sales_order (created_at DESC, id);
CREATE INDEX sales_order_department ON sales_order (department_id);

-- An order line carries its billable ledger (protocol 05): per dimension claimed + billed <= billable, in quantity
-- (the sales unit) and amount (net of tax). Every change locks the order row first.
CREATE TABLE order_line (
    id                 uuid PRIMARY KEY,
    order_id           uuid NOT NULL REFERENCES sales_order (id),
    line_no            int NOT NULL,
    kind               text NOT NULL CHECK (kind IN ('goods', 'service')),
    product_id         uuid NOT NULL,
    product_code       text NOT NULL,
    product_name       text NOT NULL,
    base_unit          text NOT NULL,
    quantity           numeric(19,6) NOT NULL CHECK (quantity > 0),
    unit               text NOT NULL,
    conversion_factor  numeric(24,12) NOT NULL,
    base_quantity      numeric(19,6) NOT NULL,
    warehouse_id       uuid,
    unit_price         numeric(19,6) NOT NULL,
    price_source       text NOT NULL CHECK (price_source IN ('price_list', 'manual')),
    price_list_item_id uuid,
    tax_rate           numeric(9,6) NOT NULL,
    net                numeric(19,4) NOT NULL,
    tax                numeric(19,4) NOT NULL,
    delivered          numeric(19,6) NOT NULL DEFAULT 0,
    billable_qty       numeric(19,6) NOT NULL DEFAULT 0,
    billable_amt       numeric(19,4) NOT NULL DEFAULT 0,
    claimed_qty        numeric(19,6) NOT NULL DEFAULT 0,
    claimed_amt        numeric(19,4) NOT NULL DEFAULT 0,
    billed_qty         numeric(19,6) NOT NULL DEFAULT 0,
    billed_amt         numeric(19,4) NOT NULL DEFAULT 0,
    UNIQUE (order_id, line_no),
    CHECK (delivered <= quantity),
    CHECK (claimed_qty + billed_qty <= billable_qty),
    CHECK (claimed_amt + billed_amt <= billable_amt)
);

-- A process over an order's reservation branches (protocol 04): a confirmation (kind confirm), the cancellation of a
-- confirmed order (cancel) or the release of what is left when the order completes (release). Cancel and release
-- processes carry the confirmed process's branch IDs again, each with the last state known.
CREATE TABLE confirmation (
    id              uuid PRIMARY KEY,
    order_id        uuid NOT NULL REFERENCES sales_order (id),
    revision        int NOT NULL,
    state           text NOT NULL CHECK (state IN ('reserving', 'committing', 'confirmed', 'rejecting', 'rejected', 'cancelling', 'cancelled')),
    kind            text NOT NULL DEFAULT 'confirm' CHECK (kind IN ('confirm', 'cancel', 'release')),
    reason          text,
    actor           text NOT NULL,
    started_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX confirmation_open ON confirmation (updated_at)
    WHERE state IN ('reserving', 'committing', 'rejecting', 'cancelling');

CREATE TABLE confirmation_branch (
    confirmation_id uuid NOT NULL REFERENCES confirmation (id),
    component       text NOT NULL CHECK (component IN ('erp/inventory', 'erp/credit')),
    branch_id       uuid NOT NULL,
    state           text NOT NULL DEFAULT 'unknown',
    reason          text,
    PRIMARY KEY (confirmation_id, component)
);
CREATE INDEX confirmation_order ON confirmation (order_id);

-- A delivery: its ID is the inventory's consumption ID.
CREATE TABLE delivery (
    id              uuid PRIMARY KEY,
    number          text NOT NULL UNIQUE,
    order_id        uuid NOT NULL REFERENCES sales_order (id),
    legal_entity_id uuid NOT NULL,
    owner_id        text NOT NULL,
    department_id   uuid,
    department_path text,
    state           text NOT NULL DEFAULT 'issuing' CHECK (state IN ('issuing', 'posted', 'failed')),
    reason          text,
    business_date   date NOT NULL,
    version         bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delivery_list ON delivery (created_at DESC, id);
CREATE INDEX delivery_order ON delivery (order_id);
CREATE INDEX delivery_department ON delivery (department_id);

CREATE TABLE delivery_line (
    delivery_id   uuid NOT NULL REFERENCES delivery (id),
    order_line_id uuid NOT NULL REFERENCES order_line (id),
    product_id    uuid NOT NULL,
    warehouse_id  uuid NOT NULL,
    quantity      numeric(19,6) NOT NULL CHECK (quantity > 0),
    base_quantity numeric(19,6) NOT NULL,
    batch         text,
    serials       text[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (delivery_id, order_line_id)
);

-- Billing claims (protocol 05's claim pattern, the billing use): the claim ID is erp/billing's invoice ID.
CREATE TABLE billing_claim (
    id         uuid PRIMARY KEY,
    order_id   uuid NOT NULL REFERENCES sales_order (id),
    state      text NOT NULL CHECK (state IN ('pending', 'confirmed', 'formalized', 'expired', 'released')),
    caller     text NOT NULL,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX billing_claim_pending ON billing_claim (expires_at) WHERE state = 'pending';

CREATE TABLE billing_claim_line (
    claim_id      uuid NOT NULL REFERENCES billing_claim (id),
    order_line_id uuid NOT NULL REFERENCES order_line (id),
    quantity      numeric(19,6) NOT NULL CHECK (quantity >= 0),
    amount        numeric(19,4) NOT NULL CHECK (amount >= 0),
    billed_qty    numeric(19,6),
    billed_amt    numeric(19,4),
    PRIMARY KEY (claim_id, order_line_id)
);
