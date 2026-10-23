package sales

import (
	"context"
	"encoding/json"

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

// onOrgUnit receives a department's event: the projection's state table decides, and a changed row rewrites the
// documents' copies in the same transaction.
func (m *Module) onOrgUnit(ctx context.Context, tx db.Tx, e events.Received) error {
	out, err := orgUnits.Apply(ctx, tx, projection.Input{RecordID: e.RecordID, Kind: string(e.Projection),
		Version: e.Version, Impl: e.Impl, Epoch: e.Epoch, Data: e.Data})
	if err != nil || !out.Changed {
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

func (s orgSource) Snapshot(ctx context.Context, cursor string) ([]projection.Record, string, error) {
	c, err := s.client()
	if err != nil {
		return nil, "", err
	}
	res, err := c.Snapshot(ctx, &orgv3.SnapshotRequest{Aggregate: orgv3.Aggregate_AGGREGATE_ORG_UNIT, Cursor: cursor},
		calls.Idempotent())
	if err != nil {
		return nil, "", err
	}
	var out []projection.Record
	for _, r := range res.GetRecords() {
		out = append(out, projection.Record{ID: r.GetId(), Version: r.GetVersion(), Data: r.GetData()})
	}
	return out, res.GetNextCursor(), nil
}

func (s orgSource) Read(ctx context.Context, ids []string) ([]projection.ReadResult, error) {
	c, err := s.client()
	if err != nil {
		return nil, err
	}
	var out []projection.ReadResult
	for len(ids) > 0 {
		n := min(len(ids), MaxReadIDs)
		res, err := c.Read(ctx, &orgv3.ReadRequest{Aggregate: orgv3.Aggregate_AGGREGATE_ORG_UNIT, Ids: ids[:n]},
			calls.Idempotent())
		if err != nil {
			return nil, err
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
	return out, nil
}

func (s orgSource) Identity(ctx context.Context) (string, string, error) {
	c, err := s.client()
	if err != nil {
		return "", "", err
	}
	res, err := c.Snapshot(ctx, &orgv3.SnapshotRequest{Aggregate: orgv3.Aggregate_AGGREGATE_ORG_UNIT, Limit: 1},
		calls.Idempotent())
	if err != nil {
		return "", "", err
	}
	return res.GetStamp().GetInstance(), res.GetStamp().GetEpoch(), nil
}

// alignOrgUnits re-fetches every department (P9.12), repairs rows waiting for a fetch, then rewrites the documents
// whose copied path differs from the projection: the safety net under the events.
func (m *Module) alignOrgUnits(ctx context.Context, _ *work.Run) error {
	src := orgSource{m}
	if err := orgUnits.Align(ctx, m.rt.DB, src); err != nil {
		return err
	}
	if _, err := orgUnits.Repair(ctx, m.rt.DB, src); err != nil {
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
