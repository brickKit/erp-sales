package documents

import (
	"context"
	"fmt"
	"strings"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	masterdata "github.com/brickKit/erp-sales/v3/internal/masterdata"
	pricing "github.com/brickKit/erp-sales/v3/internal/pricing"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/ids"
)

// The lines of quotations and orders: each number is made as docs/rules/pricing.md says.

// LineIn is a line as the user surface takes it (contract/openapi.yaml LineIn).
type LineIn struct {
	ProductID   string  `json:"product_id"`
	Quantity    string  `json:"quantity"`
	Unit        string  `json:"unit"`
	WarehouseID *string `json:"warehouse_id"`
	UnitPrice   *string `json:"unit_price"`
	TaxRate     string  `json:"tax_rate"`
}

func checkLines(in []LineIn) error {
	if len(in) == 0 || len(in) > core.MaxLines {
		return core.Invalid("lines", fmt.Sprintf("a document has 1-%d lines", core.MaxLines))
	}
	for i := range in {
		l := &in[i]
		field := fmt.Sprintf("lines[%d]", i)
		if !core.UUIDForm.MatchString(l.ProductID) {
			return core.Invalid(field+".product_id", "product_id is a product's ID")
		}
		if strings.TrimSpace(l.Unit) == "" {
			return core.Invalid(field+".unit", "unit is required")
		}
		if l.WarehouseID != nil && !core.UUIDForm.MatchString(*l.WarehouseID) {
			return core.Invalid(field+".warehouse_id", "warehouse_id is a warehouse's ID")
		}
		var err error
		if l.Quantity, err = core.Decimal(field+".quantity", l.Quantity, core.QuantityScale, true); err != nil {
			return err
		}
		if l.TaxRate, err = core.Decimal(field+".tax_rate", l.TaxRate, 6, false); err != nil {
			return err
		}
		if l.UnitPrice != nil {
			p, err := core.Decimal(field+".unit_price", *l.UnitPrice, core.PriceScale, false)
			if err != nil {
				return err
			}
			l.UnitPrice = &p
		}
	}
	return nil
}

// lineEvidence is what the lines take from the master data, read outside the transaction: each product's
// eligibility and snapshot, and each line's base quantity and conversion factor.
type lineEvidence struct {
	products map[string]masterdata.ProductInfo
	base     []string
	factor   []string
}

func evidenceOf(m *core.Base, ctx context.Context, legalEntity string, in []LineIn) (lineEvidence, error) {
	pids := make([]string, len(in))
	for i, l := range in {
		pids[i] = l.ProductID
	}
	ps, err := masterdata.ProductsOf(m, ctx, pids, legalEntity)
	if err != nil {
		return lineEvidence{}, err
	}
	ev := lineEvidence{products: ps, base: make([]string, len(in)), factor: make([]string, len(in))}
	for i, l := range in {
		p := ps[l.ProductID]
		if p.Stocked && l.WarehouseID == nil {
			return ev, core.Invalid(fmt.Sprintf("lines[%d].warehouse_id", i), "a goods line needs its warehouse")
		}
		if ev.base[i], ev.factor[i], err = masterdata.Convert(m, ctx, l.ProductID, l.Quantity, l.Unit, p.BaseUnit); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// head is a document's header as its lines need it.
type head struct {
	Le       masterdata.LegalEntity `json:"-"`
	Cust     masterdata.Customer    `json:"-"`
	Currency string                 `json:"-"`
	scale    int32
	Owner    string  `json:"-"`
	DeptID   *string `json:"-"`
	DeptPath *string `json:"-"`
}

// BuildLines makes the lines of a document. A line without a unit price takes the price of the line it replaces (the
// same product and unit), else the price list's on the date; a price given by hand is manual.
func BuildLines(ctx context.Context, tx db.Tx, h head, date string, in []LineIn, ev lineEvidence, prev []core.QuoteLine) ([]core.OrderLine, error) {
	out := make([]core.OrderLine, len(in))
	for i, l := range in {
		p := ev.products[l.ProductID]
		ol := core.OrderLine{QuoteLine: core.QuoteLine{ID: ids.New().String(), LineNo: i + 1, Kind: "service",
			Product:  core.ProductSnap{ID: p.ID, Code: p.Code, Name: p.Name, BaseUnit: p.BaseUnit},
			Quantity: l.Quantity, Unit: l.Unit, ConversionFactor: ev.factor[i], BaseQuantity: ev.base[i],
			TaxRate: l.TaxRate, TaxSource: "manual"}}
		if p.Stocked {
			ol.Kind, ol.WarehouseID = "goods", l.WarehouseID
		}
		switch {
		case l.UnitPrice != nil:
			ol.UnitPrice, ol.PriceSource = *l.UnitPrice, "manual"
		default:
			for _, o := range prev {
				if o.Product.ID == l.ProductID && o.Unit == l.Unit {
					ol.UnitPrice, ol.PriceSource, ol.PriceListItemID = o.UnitPrice, o.PriceSource, o.PriceListItemID
					break
				}
			}
			if ol.UnitPrice == "" {
				price, item, err := pricing.PriceOf(ctx, tx, h.Le.ID, h.Currency, l.ProductID, l.Unit, p.BaseUnit, ev.factor[i], date)
				if err != nil {
					return nil, err
				}
				if item == nil {
					return nil, core.ErrNoPrice.New(fmt.Sprintf("line %d: no price for product %s in %s on %s", i+1, p.Code, l.Unit, date))
				}
				ol.UnitPrice, ol.PriceSource, ol.PriceListItemID = price, "price_list", item
			}
		}
		ol.Net = core.Times(ol.Quantity, ol.UnitPrice, h.scale)
		ol.Tax = core.Times(ol.Net, ol.TaxRate, h.scale)
		out[i] = ol
	}
	return out, nil
}

// WriteLines replaces a draft's lines and its totals (the sums of the rounded lines, never rounded again).
func WriteLines(ctx context.Context, tx db.Tx, table, docID string, lines []core.OrderLine) error {
	doc := map[string]string{"quotation": "quotation_id", "sales_order": "order_id"}[table]
	lt := map[string]string{"quotation": "quotation_line", "sales_order": "order_line"}[table]
	if _, err := tx.Exec(ctx, `DELETE FROM `+lt+` WHERE `+doc+` = $1`, docID); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO `+lt+` (id, `+doc+`, line_no, kind, product_id, product_code, product_name,
			base_unit, quantity, unit, conversion_factor, base_quantity, warehouse_id, unit_price, price_source,
			price_list_item_id, tax_rate, net, tax) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11::numeric,
			$12::numeric, $13, $14::numeric, $15, $16, $17::numeric, $18::numeric, $19::numeric)`,
			l.ID, docID, l.LineNo, l.Kind, l.Product.ID, l.Product.Code, l.Product.Name, l.Product.BaseUnit, l.Quantity,
			l.Unit, l.ConversionFactor, l.BaseQuantity, l.WarehouseID, l.UnitPrice, l.PriceSource, l.PriceListItemID,
			l.TaxRate, l.Net, l.Tax); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE `+table+` d SET net = coalesce((SELECT sum(net) FROM `+lt+` WHERE `+doc+` = d.id), 0),
		tax = coalesce((SELECT sum(tax) FROM `+lt+` WHERE `+doc+` = d.id), 0) WHERE d.id = $1`, docID)
	return err
}
