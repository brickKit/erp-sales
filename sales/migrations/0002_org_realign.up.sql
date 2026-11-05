-- One row while an alignment of the department projection is queued (3.0.25): the department events of a newer
-- epoch of mdm/org (it went through a full recovery) are not applied before the projection aligns, and every such
-- event would otherwise queue an alignment of its own.
CREATE TABLE org_realign (
    single    boolean PRIMARY KEY DEFAULT true CHECK (single),
    queued_at timestamptz NOT NULL DEFAULT now()
);
