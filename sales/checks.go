package sales

import (
	"context"

	"github.com/brickKit/be-sdk-go/v3/recovery"
	core "github.com/brickKit/erp-sales/v3/internal/core"
	recoverycheck "github.com/brickKit/erp-sales/v3/internal/recoverycheck"
)

// RecoveryChecks registers what a full recovery asks this member (be-protocol chapter 14): whether the objects this
// member audits are still here, at which version, and the checks it reports.
func (m *Module) RecoveryChecks(c *recovery.Checks) error {
	c.Presence(core.Quotations, recovery.TablePresence("quotation", "id", "version"))
	c.Presence(core.Orders, recovery.TablePresence("sales_order", "id", "version"))
	c.Presence(core.Deliveries, recovery.TablePresence("delivery", "id", "version"))
	c.Presence(core.PriceLists, recovery.TablePresence("price_list", "id", "version"))
	if err := c.Add(recovery.Check{Name: recoverycheck.CheckReservations, Basis: "装配项目协议 04（本组件是协调者，替两边报告）；协议 15 第 8 步",
		Run: func(ctx context.Context, read recovery.Read) (recovery.Result, error) {
			return recoverycheck.ReservationsCheck(m.b, ctx, read)
		}}); err != nil {
		return err
	}
	return nil
}
