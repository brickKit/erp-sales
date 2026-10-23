package sales

import (
	"context"
	"fmt"
	"strings"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/money"
	"github.com/cockroachdb/apd/v3"
)

// The lines of quotations and orders: each number is made as docs/rules/pricing.md says.

// lineIn is a line as the user surface takes it (contract/openapi.yaml LineIn).
type lineIn struct {
	ProductID   string  `json:"product_id"`
	Quantity    string  `json:"quantity"`
	Unit        string  `json:"unit"`
	WarehouseID *string `json:"warehouse_id"`
	UnitPrice   *string `json:"unit_price"`
	TaxRate     string  `json:"tax_rate"`
}

func checkLines(in []lineIn) error {
	if len(in) == 0 || len(in) > MaxLines {
		return invalid("lines", fmt.Sprintf("a document has 1-%d lines", MaxLines))
	}
	for i := range in {
		l := &in[i]
		field := fmt.Sprintf("lines[%d]", i)
		if !uuidForm.MatchString(l.ProductID) {
			return invalid(field+".product_id", "product_id is a product's ID")
		}
		if strings.TrimSpace(l.Unit) == "" {
			return invalid(field+".unit", "unit is required")
		}
		if l.WarehouseID != nil && !uuidForm.MatchString(*l.WarehouseID) {
			return invalid(field+".warehouse_id", "warehouse_id is a warehouse's ID")
		}
		var err error
		if l.Quantity, err = decimal(field+".quantity", l.Quantity, QuantityScale, true); err != nil {
			return err
		}
		if l.TaxRate, err = decimal(field+".tax_rate", l.TaxRate, 6, false); err != nil {
			return err
		}
		if l.UnitPrice != nil {
			p, err := decimal(field+".unit_price", *l.UnitPrice, PriceScale, false)
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
	products map[string]productInfo
	base     []string
	factor   []string
}

func (m *Module) evidenceOf(ctx context.Context, legalEntity string, in []lineIn) (lineEvidence, error) {
	pids := make([]string, len(in))
	for i, l := range in {
		pids[i] = l.ProductID
	}
	ps, err := m.productsOf(ctx, pids, legalEntity)
	if err != nil {
		return lineEvidence{}, err
	}
	ev := lineEvidence{products: ps, base: make([]string, len(in)), factor: make([]string, len(in))}
	for i, l := range in {
		p := ps[l.ProductID]
		if p.Stocked && l.WarehouseID == nil {
			return ev, invalid(fmt.Sprintf("lines[%d].warehouse_id", i), "a goods line needs its warehouse")
		}
		if ev.base[i], ev.factor[i], err = m.convert(ctx, l.ProductID, l.Quantity, l.Unit, p.BaseUnit); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// times is a × b rounded half away from zero to scale places.
func times(a, b string, scale int32) string {
	x, _, _ := apd.NewFromString(a)
	y, _, _ := apd.NewFromString(b)
	var r apd.Decimal
	_, _ = apd.BaseContext.WithPrecision(80).Mul(&r, x, y)
	return money.Format(&r, scale)
}

// head is a document's header as its lines need it.
type head struct {
	le       legalEntity
	cust     customer
	currency string
	scale    int32
	owner    string
	deptID   *string
	deptPath *string
}

// buildLines makes the lines of a document. A line without a unit price takes the price of the line it replaces (the
// same product and unit), else the price list's on the date; a price given by hand is manual.
func buildLines(ctx context.Context, tx db.Tx, h head, date string, in []lineIn, ev lineEvidence, prev []QuoteLine) ([]OrderLine, error) {
	out := make([]OrderLine, len(in))
	for i, l := range in {
		p := ev.products[l.ProductID]
		ol := OrderLine{QuoteLine: QuoteLine{ID: ids.New().String(), LineNo: i + 1, Kind: "service",
			Product:  ProductSnap{ID: p.ID, Code: p.Code, Name: p.Name, BaseUnit: p.BaseUnit},
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
				price, item, err := priceOf(ctx, tx, h.le.ID, h.currency, l.ProductID, l.Unit, p.BaseUnit, ev.factor[i], date)
				if err != nil {
					return nil, err
				}
				if item == nil {
					return nil, errNoPrice.New(fmt.Sprintf("line %d: no price for product %s in %s on %s", i+1, p.Code, l.Unit, date))
				}
				ol.UnitPrice, ol.PriceSource, ol.PriceListItemID = price, "price_list", item
			}
		}
		ol.Net = times(ol.Quantity, ol.UnitPrice, h.scale)
		ol.Tax = times(ol.Net, ol.TaxRate, h.scale)
		out[i] = ol
	}
	return out, nil
}

// writeLines replaces a draft's lines and its totals (the sums of the rounded lines, never rounded again).
func writeLines(ctx context.Context, tx db.Tx, table, docID string, lines []OrderLine) error {
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
