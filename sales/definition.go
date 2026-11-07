// Package sales is the sales component: quotations, orders and their confirmation (the coordinator of protocol 04),
// deliveries through the inventory, price lists, and the billable ledger erp/billing claims from (protocol 05).
package sales

import (
	"embed"
	"io/fs"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	orders "github.com/brickKit/erp-sales/v3/internal/orders"

	"github.com/brickKit/be-sdk-go/v3/component"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	"github.com/brickKit/be-sdk-go/v3/tiering"
)

// Identity.
const (
	Version  = "3.0.40"
	GRPCPort = 9094
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrations() fs.FS {
	sub, _ := fs.Sub(migrationFiles, "migrations")
	return sub
}

// Definition is the component as the launcher and the shells take it.
func Definition() component.Definition {
	return component.Definition{ID: core.ID, Version: Version, Migrations: migrations(), GRPCPort: GRPCPort, Subscribes: true,
		Streams: []events.Stream{{Name: core.Stream, Partitions: 4}}, Authz: core.Definitions(), Resend: core.OpResend, Recovery: core.OpRecover,
		Replay: core.OpReplay, Tiering: []tiering.Class{orders.OrdersClass()},
		New: func(rt *runtime.Runtime) (component.Module, error) { return New(rt) }}
}
