package sales

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brickKit/be-sdk-go/v3/calls"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/projection"
	"github.com/brickKit/be-sdk-go/v3/work"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
)

// The department projection (protocol 02): mdm/org's departments, kept for one fact only, the org-tree path of the
// department dimension (security 07). Documents copy their department's path into department_path, the column the
// list compiler compares; when a department moves, its event rewrites the copies.

var orgUnits = projection.Projection{Name: OrgUnitsProject}

// repath rewrites the department path on every document of the department.
func repath(ctx context.Context, tx db.Tx, id, path string) error {
	for _, table := range []string{"quotation", "sales_order", "delivery"} {
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET department_path = $2 WHERE department_id = $1
			AND department_path IS DISTINCT FROM $2`, id, path); err != nil {
			return err
		}
	}
	return nil
}

const jobRepairOrgUnits = "repair-org-units"

// jobRealignOrgUnits aligns the department projection now, not at the hourly alignment: mdm/org's events of a newer
// epoch (it went through a full recovery, protocol 15) wait for it (protocol 02; be-sdk-go's NewerEpoch).
const jobRealignOrgUnits = "realign-org-units"

// onOrgUnit receives a department's event: the projection's state table decides, and a changed row rewrites the
// documents' copies in the same transaction. An action this version does not know (a later mdm/org's) changes the
// department somehow, but its payload's shape is not known: the row is marked incomplete without taking anything from
// it, and a repair fetches the department (protocol P6.33, protocol 02 PJ36).
func (m *Module) onOrgUnit(ctx context.Context, tx db.Tx, e events.Received) error {
	in := projection.Input{RecordID: e.RecordID, Kind: string(e.Projection), Version: e.Version, Impl: e.Impl,
		Epoch: e.Epoch, Data: e.Data}
	if e.Type != OrgUnitChanged {
		in.Kind, in.Data = projection.Partial, json.RawMessage(`{}`)
	}
	out, err := orgUnits.Apply(ctx, tx, in)
	if err == nil && out.Skipped == projection.NewerEpoch {
		return m.realignSoon(ctx, tx)
	}
	if err != nil || !out.Changed {
		return err
	}
	if out.Fetch {
		_, err := m.rt.Work.Enqueue(ctx, tx, jobRepairOrgUnits, struct{}{})
		return err
	}
	var u orgUnit
	if err := json.Unmarshal(e.Data, &u); err != nil {
		return err
	}
	return repath(ctx, tx, u.ID, u.Path)
}

// department answers a department from the projection, filling it from mdm/org when the projection does not have it
// yet (a document may name a department created a moment ago).
func (m *Module) department(ctx context.Context, id string) (orgUnit, bool, error) {
	var u orgUnit
	var found bool
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		state, _, data, ok, err := orgUnits.Get(ctx, tx, id)
		if err != nil || !ok || state != projection.Complete {
			return err
		}
		found = true
		return json.Unmarshal(data, &u)
	})
	if err != nil || found {
		return u, found, err
	}
	u, version, impl, epoch, exists, err := m.orgUnitRead(ctx, id)
	if err != nil || !exists {
		return u, false, err
	}
	data, _ := json.Marshal(u)
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := orgUnits.Apply(ctx, tx, projection.Input{RecordID: id, Kind: projection.Full, Version: version,
			Impl: impl, Epoch: epoch, Data: data})
		return err
	})
	return u, true, err
}

// orgSource is mdm/org's source methods for the department aggregate, as the projection reads them.
type orgSource struct{ m *Module }

func (s orgSource) client() (orgv3.SourceServiceClient, error) {
	c, err := s.m.conn(Org)
	if err != nil {
		return nil, err
	}
	return orgv3.NewSourceServiceClient(c), nil
}

// identityOf is who answered (P9.18): mdm/org's stamp. Its source methods answer from its primary only (they have
// no replica answer), so the stamp declares the primary.
func identityOf(st *orgv3.Stamp) projection.Identity {
	return projection.Identity{Deployment: st.GetDeployment(), Impl: st.GetInstance(), Epoch: st.GetEpoch(), Primary: true}
}

func (s orgSource) Snapshot(ctx context.Context, cursor string) ([]projection.Record, string, projection.Identity, error) {
	c, err := s.client()
	if err != nil {
		return nil, "", projection.Identity{}, err
	}
	res, err := c.Snapshot(ctx, &orgv3.SnapshotRequest{Aggregate: orgv3.Aggregate_AGGREGATE_ORG_UNIT, Cursor: cursor},
		calls.Idempotent())
	if err != nil {
		return nil, "", projection.Identity{}, err
	}
	var out []projection.Record
	for _, r := range res.GetRecords() {
		out = append(out, projection.Record{ID: r.GetId(), Version: r.GetVersion(), Data: r.GetData()})
	}
	return out, res.GetNextCursor(), identityOf(res.GetStamp()), nil
}

// Read reads in batches of MaxReadIDs; every batch must come from the same source, or the answer is not one.
func (s orgSource) Read(ctx context.Context, ids []string) ([]projection.ReadResult, projection.Identity, error) {
	var from projection.Identity
	c, err := s.client()
	if err != nil {
		return nil, from, err
	}
	var out []projection.ReadResult
	for first := true; len(ids) > 0; first = false {
		n := min(len(ids), MaxReadIDs)
		res, err := c.Read(ctx, &orgv3.ReadRequest{Aggregate: orgv3.Aggregate_AGGREGATE_ORG_UNIT, Ids: ids[:n]},
			calls.Idempotent())
		if err != nil {
			return nil, from, err
		}
		if id := identityOf(res.GetStamp()); first {
			from = id
		} else if id != from {
			return nil, from, fmt.Errorf("%w: one read answered by %+v and by %+v", projection.ErrOtherSource, from, id)
		}
		for _, r := range res.GetResults() {
			st := projection.Purged
			switch r.GetState() {
			case orgv3.ReadState_READ_STATE_EXISTS:
				st = projection.Exists
			case orgv3.ReadState_READ_STATE_TOMBSTONE:
				st = projection.Gone
			}
			out = append(out, projection.ReadResult{ID: r.GetId(), State: st, Version: r.GetVersion(), Data: r.GetData()})
		}
		ids = ids[n:]
	}
	return out, from, nil
}

// alignOrgUnits re-fetches every department (P9.12), repairs rows waiting for a fetch, then rewrites the documents
// whose copied path differs from the projection: the safety net under the events.
// realignSoon queues one alignment of the projection (one at a time: the row in org_realign), in the event's
// transaction; the alignment takes the source's current state, the skipped event's change with it.
func (m *Module) realignSoon(ctx context.Context, tx db.Tx) error {
	tag, err := tx.Exec(ctx, "INSERT INTO org_realign DEFAULT VALUES ON CONFLICT DO NOTHING")
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = m.rt.Work.Enqueue(ctx, tx, jobRealignOrgUnits, struct{}{})
	return err
}

// realignOrgUnits is the queued alignment: the row goes first, so an event of the new epoch arriving meanwhile queues
// another one.
func (m *Module) realignOrgUnits(ctx context.Context, r *work.Run) error {
	if err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM org_realign")
		return err
	}); err != nil {
		return err
	}
	return m.alignOrgUnits(ctx, r)
}

func (m *Module) alignOrgUnits(ctx context.Context, r *work.Run) error {
	if err := orgUnits.Align(ctx, m.rt.DB, orgSource{m}); err != nil {
		return err
	}
	return m.repairOrgUnits(ctx, r)
}

// repairOrgUnits fetches the departments waiting for a fetch, then rewrites the documents whose copied path differs
// from the projection.
func (m *Module) repairOrgUnits(ctx context.Context, _ *work.Run) error {
	if _, err := orgUnits.Repair(ctx, m.rt.DB, orgSource{m}); err != nil {
		return err
	}
	return m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		for _, table := range []string{"quotation", "sales_order", "delivery"} {
			if _, err := tx.Exec(ctx, `UPDATE `+table+` d SET department_path = p.data->>'path' FROM be_projection p
				WHERE p.projection = $1 AND p.record_id = d.department_id::text AND p.state = 'complete'
				AND d.department_path IS DISTINCT FROM p.data->>'path'`, OrgUnitsProject); err != nil {
				return err
			}
		}
		return nil
	})
}
