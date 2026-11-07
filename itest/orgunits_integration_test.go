//go:build integration

package itest

import (
	"maps"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	core "github.com/brickKit/erp-sales/v3/internal/core"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// departments: Bob reads the orders of department 1 and below; department 2 moves under it and its orders follow.
func (fx *fixture) departments() {
	t := fx.t
	inD1 := fx.newOrder(map[string]any{"department_id": fx.d1, "lines": []any{fx.service()}})["id"]
	inD2 := fx.newOrder(map[string]any{"department_id": fx.d2, "lines": []any{fx.service()}})["id"]
	fx.w.Grant(sectest.Bob, sdkauthz.Segment{ResourceType: core.Orders, Operation: core.OpOrderRead, ReadFields: core.DocumentFields,
		Atoms: map[string]sdkauthz.Atom{core.DimLegalEntity: {Kind: "all"}, core.DimOwner: {Kind: "all"},
			core.DimDepartment: {Kind: "tree", Items: []sdkauthz.TreeItem{{Path: fx.le + "/" + fx.d1, Mode: "subtree"}}}},
		Valid: sdkauthz.Window{From: "2026-01-01T00:00:00Z"}})
	bob := client{t, fx.w.Bearer(sectest.Bob)}
	seen := func() map[any]bool {
		_, page := bob.do("GET", "/api/sales/orders", nil)
		out := map[any]bool{}
		listed, _ := page["orders"].([]any)
		for _, o := range listed {
			out[at(o, "id")] = true
		}
		return out
	}
	require.Eventually(t, func() bool { return seen()[inD1] }, 30*time.Second, 200*time.Millisecond)
	assert.False(t, seen()[inD2], "department 2 is outside Bob's tree")
	code, _ := bob.do("GET", "/api/sales/orders/"+inD2.(string), nil)
	assert.Equal(t, 404, code)
	moved := fx.unit(fx.d2, fx.le+"/"+fx.d1+"/"+fx.d2, 2)
	moved["parent_id"] = fx.d1
	fx.org.mu.Lock()
	fx.org.units[fx.d2] = moved
	fx.org.mu.Unlock()
	fx.publishOrgUnit(moved)
	require.Eventually(t, func() bool { return seen()[inD2] }, 30*time.Second, 200*time.Millisecond,
		"the department's move rewrites the copied path")
	fx.laterOrgEvents(moved)

	// mdm/org went through a full recovery (protocol 15): its epoch advanced and its department went back to an
	// earlier version, now outside department 1. Its events of the new epoch are not applied before the projection
	// aligns to it (protocol 02, be-sdk-go's NewerEpoch): the projection aligns at once, not at the hourly alignment,
	// and the copied path follows
	back := fx.unit(fx.d2, fx.le+"/"+fx.d2, 2)
	fx.org.mu.Lock()
	orgStamp.Epoch = "2.1"
	fx.org.units[fx.d2] = back
	fx.org.mu.Unlock()
	fx.publishOrgUnit(back)
	require.Eventually(t, func() bool { return !seen()[inD2] }, 30*time.Second, 200*time.Millisecond,
		"after the source's recovery the projection aligns to its new epoch and the order leaves Bob's tree")
}

// laterOrgEvents: mdm/org later publishes an action this version does not know and keeps publishing its legal
// entities on the same stream (protocol P6.33, protocol 02 PJ36). The unknown action marks the department incomplete
// and fetches it, without applying a payload whose shape it does not know; the legal entity is not a department.
func (fx *fixture) laterOrgEvents(unit map[string]any) {
	t := fx.t
	renamed := maps.Clone(unit)
	renamed["name"], renamed["version"] = "改名后的部门", int64(3)
	fx.org.mu.Lock()
	fx.org.units[fx.d2] = renamed
	fx.org.mu.Unlock()
	fx.publishOrg("mdm.org_unit.renamed.v1", fx.d2, 3, events.Full, []byte(`{"unit_id":"`+fx.d2+`","new_name":"x"}`))
	fx.publishOrg("mdm.legal_entity.changed.v1", fx.le, 7, events.Full, []byte(`{"id":"`+fx.le+`","name":"法人"}`))
	row := func(id string) (state string, version int64, name string) {
		_ = fx.admin.QueryRow(fx.ctx, "SELECT state, version, coalesce(data->>'name', '') FROM "+itSchema+
			".be_projection WHERE projection = $1 AND record_id = $2", core.OrgUnitsProject, id).Scan(&state, &version, &name)
		return
	}
	require.Eventually(t, func() bool {
		state, version, name := row(fx.d2)
		return state == "complete" && version == 3 && name == "改名后的部门"
	}, 30*time.Second, 200*time.Millisecond, "the unknown action is fetched from mdm/org, not taken from its payload")
	state, _, _ := row(fx.le)
	assert.Empty(t, state, "a legal entity's event is not a department")
	var dead int
	require.NoError(t, fx.admin.QueryRow(fx.ctx, "SELECT count(*) FROM "+itSchema+".be_dead_letter").Scan(&dead))
	assert.Zero(t, dead, "no event of the stream is dead-lettered")
}
