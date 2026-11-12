-- crm/opportunity's quote requests (3.0.50, design QR01-QR05): a quotation carries the opportunity; a draft made from
-- a request carries the request and the products it suggested, and may have no lines. An order reads its opportunity
-- from its quotation (quotations are kept online and never deleted): a column on sales_order would change the
-- structure of the archived orders' class, whose older units the component cannot register a converter for.
ALTER TABLE quotation ADD COLUMN opportunity_ref uuid;
ALTER TABLE quotation ADD COLUMN quote_request_id uuid UNIQUE;
ALTER TABLE quotation ADD COLUMN requested_products jsonb NOT NULL DEFAULT '[]'::jsonb;

-- Every quote request received, by its fact ID: one draft however often it is delivered (QR01).
CREATE TABLE quote_request_in (
    id             uuid PRIMARY KEY,
    opportunity_id uuid NOT NULL,
    payload        jsonb NOT NULL,
    state          text NOT NULL DEFAULT 'received' CHECK (state IN ('received', 'drafted', 'rejected')),
    quotation_id   uuid REFERENCES quotation (id),
    reason         text,
    received_at    timestamptz NOT NULL DEFAULT now(),
    decided_at     timestamptz
);
