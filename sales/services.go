package sales

import (
	"context"

	"github.com/brickKit/be-sdk-go/v3/tiering"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
	"github.com/brickKit/erp-sales/v3/internal/billingsource"
	"github.com/brickKit/erp-sales/v3/internal/orders"
)

// The system surface's services and the archive's registration: methods the SDK calls on the module, each answered
// by the business module that owns it (internal/billingsource: the billable ledger's source and the order read;
// internal/orders: what keeps an ended order online).

func (m *Module) Ledger(ctx context.Context, in *salesv3.LedgerRequest) (*salesv3.LedgerResponse, error) {
	return billingsource.Ledger(m.b, ctx, in)
}

func (m *Module) Claim(ctx context.Context, in *salesv3.ClaimRequest) (*salesv3.ClaimResponse, error) {
	return billingsource.Claim(m.b, ctx, in)
}

func (m *Module) Renew(ctx context.Context, in *salesv3.RenewRequest) (*salesv3.RenewResponse, error) {
	return billingsource.Renew(m.b, ctx, in)
}

func (m *Module) Confirm(ctx context.Context, in *salesv3.ConfirmRequest) (*salesv3.ConfirmResponse, error) {
	return billingsource.Confirm(m.b, ctx, in)
}

func (m *Module) Formalize(ctx context.Context, in *salesv3.FormalizeRequest) (*salesv3.FormalizeResponse, error) {
	return billingsource.Formalize(m.b, ctx, in)
}

func (m *Module) Release(ctx context.Context, in *salesv3.ReleaseRequest) (*salesv3.ReleaseResponse, error) {
	return billingsource.Release(m.b, ctx, in)
}

func (m *Module) Reverse(ctx context.Context, in *salesv3.ReverseRequest) (*salesv3.ReverseResponse, error) {
	return billingsource.Reverse(m.b, ctx, in)
}

func (m *Module) Read(ctx context.Context, in *salesv3.ReadRequest) (*salesv3.ReadResponse, error) {
	return billingsource.Read(m.b, ctx, in)
}

// Tiering registers what keeps an ended order online (assembly protocol 11, P15.12).
func (m *Module) Tiering(r *tiering.Registry) error { return orders.Tiering(m.b, r) }
