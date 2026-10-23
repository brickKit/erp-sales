package sales

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/jackc/pgx/v5"
)

// Customer is a document's customer snapshot (protocol 07: name and tax ID as they were).
type Customer struct {
	PartnerID string  `json:"partner_id"`
	Name      string  `json:"name"`
	TaxID     *string `json:"tax_id"`
}

// ProductSnap is a line's product snapshot.
type ProductSnap struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	BaseUnit string `json:"base_unit"`
}

// Measure is the two dimensions of the billable ledger.
type Measure struct {
	Quantity string `json:"quantity"`
	Amount   string `json:"amount"`
}

// Totals are a document's sums.
type Totals struct {
	Net   string `json:"net"`
	Tax   string `json:"tax"`
	Gross string `json:"gross"`
}

// Rate is an order's exchange rate snapshot (protocol 14).
type Rate struct {
	Rate          string `json:"rate"`
	Date          string `json:"date"`
	Type          string `json:"type"`
	RecordID      string `json:"record_id"`
	RecordVersion int64  `json:"record_version"`
}

// QuoteLine is a quotation line (docs/rules/pricing.md says how each number is made).
type QuoteLine struct {
	ID               string      `json:"id"`
	LineNo           int         `json:"line_no"`
	Kind             string      `json:"kind"`
	Product          ProductSnap `json:"product"`
	Quantity         string      `json:"quantity"`
	Unit             string      `json:"unit"`
	ConversionFactor string      `json:"conversion_factor"`
	BaseQuantity     string      `json:"base_quantity"`
	UnitPrice        string      `json:"unit_price"`
	PriceSource      string      `json:"price_source"`
	PriceListItemID  *string     `json:"price_list_item_id"`
	TaxRate          string      `json:"tax_rate"`
	TaxSource        string      `json:"tax_source"`
	Net              string      `json:"net"`
	Tax              string      `json:"tax"`
}

// OrderLine is an order line with its warehouse, delivery progress and billable ledger.
type OrderLine struct {
	QuoteLine
	WarehouseID *string `json:"warehouse_id"`
	Delivered   string  `json:"delivered_quantity"`
	Billable    Measure `json:"billable"`
	Claimed     Measure `json:"claimed"`
	Billed      Measure `json:"billed"`
}

// Quotation is a quotation's complete state (erp.quotation.changed.v1).
type Quotation struct {
	ID             string      `json:"id"`
	Number         string      `json:"number"`
	LegalEntityID  string      `json:"legal_entity_id"`
	Customer       Customer    `json:"customer"`
	Currency       string      `json:"currency"`
	OwnerID        string      `json:"owner_id"`
	DepartmentID   *string     `json:"department_id"`
	State          string      `json:"state"`
	ValidUntil     *string     `json:"valid_until"`
	OrderID        *string     `json:"order_id"`
	OpportunityRef *string     `json:"opportunity_ref"`
	Lines          []QuoteLine `json:"lines"`
	Totals         Totals      `json:"totals"`
	Version        int64       `json:"version"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`

	DepartmentPath *string `json:"-"`
}

// Order is a sales order's complete state (erp.sales_order.changed.v1; also OrderService.Read's data).
type Order struct {
	ID             string      `json:"id"`
	Number         string      `json:"number"`
	LegalEntityID  string      `json:"legal_entity_id"`
	Customer       Customer    `json:"customer"`
	Currency       string      `json:"currency"`
	OwnerID        string      `json:"owner_id"`
	DepartmentID   *string     `json:"department_id"`
	State          string      `json:"state"`
	BusinessDate   string      `json:"business_date"`
	ExchangeRate   *Rate       `json:"exchange_rate"`
	Revision       int         `json:"revision"`
	QuotationID    *string     `json:"quotation_id"`
	ConfirmationID *string     `json:"confirmation_id"`
	Reason         *string     `json:"reason"`
	Lines          []OrderLine `json:"lines"`
	Totals         Totals      `json:"totals"`
	Version        int64       `json:"version"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`

	DepartmentPath *string `json:"-"`
}

// DeliveryLine is one order line of a delivery.
type DeliveryLine struct {
	OrderLineID  string   `json:"order_line_id"`
	ProductID    string   `json:"product_id"`
	WarehouseID  string   `json:"warehouse_id"`
	Quantity     string   `json:"quantity"`
	BaseQuantity string   `json:"base_quantity"`
	Batch        *string  `json:"batch"`
	Serials      []string `json:"serials"`
}

// Delivery is a delivery's complete state (erp.delivery.changed.v1).
type Delivery struct {
	ID            string         `json:"id"`
	Number        string         `json:"number"`
	OrderID       string         `json:"order_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	State         string         `json:"state"`
	Reason        *string        `json:"reason"`
	BusinessDate  string         `json:"business_date"`
	Lines         []DeliveryLine `json:"lines"`
	OwnerID       string         `json:"owner_id"`
	DepartmentID  *string        `json:"department_id"`
	Version       int64          `json:"version"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`

	DepartmentPath *string `json:"-"`
}

// PriceList is a price list without its items (erp.price_list.changed.v1).
type PriceList struct {
	ID            string `json:"id"`
	LegalEntityID string `json:"legal_entity_id"`
	Currency      string `json:"currency"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	State         string `json:"state"`
	Version       int64  `json:"version"`
	UpdatedAt     string `json:"updated_at"`
}

// PriceListItem is one price (erp.price_list_item.changed.v1).
type PriceListItem struct {
	ID          string  `json:"id"`
	PriceListID string  `json:"price_list_id"`
	ProductID   string  `json:"product_id"`
	Unit        string  `json:"unit"`
	Price       string  `json:"price"`
	ValidFrom   string  `json:"valid_from"`
	ValidTo     *string `json:"valid_to"`
	Version     int64   `json:"version"`
	UpdatedAt   string  `json:"updated_at"`
}

const lineJSON = `'id', l.id::text, 'line_no', l.line_no, 'kind', l.kind, 'product', jsonb_build_object('id', l.product_id::text,
	'code', l.product_code, 'name', l.product_name, 'base_unit', l.base_unit), 'quantity', trim_scale(l.quantity)::text,
	'unit', l.unit, 'conversion_factor', trim_scale(l.conversion_factor)::text, 'base_quantity', trim_scale(l.base_quantity)::text,
	'unit_price', trim_scale(l.unit_price)::text, 'price_source', l.price_source, 'price_list_item_id', l.price_list_item_id::text,
	'tax_rate', trim_scale(l.tax_rate)::text, 'tax_source', 'manual', 'net', trim_scale(l.net)::text, 'tax', trim_scale(l.tax)::text`

const (
	quotationCols = `q.id::text, q.number, q.legal_entity_id::text, q.customer_id::text, q.customer_name, q.customer_tax_id,
		q.currency, q.owner_id, q.department_id::text, q.department_path, q.state, q.valid_until::text, q.order_id::text,
		trim_scale(q.net)::text, trim_scale(q.tax)::text, trim_scale(q.net + q.tax)::text, q.version, q.created_at, q.updated_at,
		COALESCE((SELECT jsonb_agg(jsonb_build_object(` + lineJSON + `) ORDER BY l.line_no) FROM quotation_line l
			WHERE l.quotation_id = q.id), '[]'::jsonb)`
	orderCols = `o.id::text, o.number, o.legal_entity_id::text, o.customer_id::text, o.customer_name, o.customer_tax_id,
		o.currency, o.owner_id, o.department_id::text, o.department_path, o.state, o.business_date::text,
		trim_scale(o.rate)::text, o.rate_date::text, o.rate_type, o.rate_record_id::text, o.rate_record_version, o.revision,
		o.quotation_id::text, o.confirmation_id::text, o.reason, trim_scale(o.net)::text, trim_scale(o.tax)::text,
		trim_scale(o.net + o.tax)::text, o.version, o.created_at, o.updated_at,
		COALESCE((SELECT jsonb_agg(jsonb_build_object(` + lineJSON + `, 'warehouse_id', l.warehouse_id::text,
			'delivered_quantity', trim_scale(l.delivered)::text,
			'billable', jsonb_build_object('quantity', trim_scale(l.billable_qty)::text, 'amount', trim_scale(l.billable_amt)::text),
			'claimed', jsonb_build_object('quantity', trim_scale(l.claimed_qty)::text, 'amount', trim_scale(l.claimed_amt)::text),
			'billed', jsonb_build_object('quantity', trim_scale(l.billed_qty)::text, 'amount', trim_scale(l.billed_amt)::text))
			ORDER BY l.line_no) FROM order_line l WHERE l.order_id = o.id), '[]'::jsonb)`
	deliveryCols = `d.id::text, d.number, d.order_id::text, d.legal_entity_id::text, d.state, d.reason, d.business_date::text,
		d.owner_id, d.department_id::text, d.department_path, d.version, d.created_at, d.updated_at,
		COALESCE((SELECT jsonb_agg(jsonb_build_object('order_line_id', l.order_line_id::text, 'product_id', l.product_id::text,
			'warehouse_id', l.warehouse_id::text, 'quantity', trim_scale(l.quantity)::text,
			'base_quantity', trim_scale(l.base_quantity)::text, 'batch', l.batch, 'serials', to_jsonb(l.serials))
			ORDER BY l.order_line_id) FROM delivery_line l WHERE l.delivery_id = d.id), '[]'::jsonb)`
	priceListCols = `p.id::text, p.legal_entity_id::text, p.currency, p.code, p.name, p.state, p.version, p.updated_at`
	priceItemCols = `i.id::text, i.price_list_id::text, i.product_id::text, i.unit, trim_scale(i.price)::text,
		i.valid_from::text, i.valid_to::text, i.version, i.updated_at`
)

func scanQuotation(row pgx.Row) (Quotation, error) {
	var q Quotation
	var created, updated time.Time
	var lines []byte
	err := row.Scan(&q.ID, &q.Number, &q.LegalEntityID, &q.Customer.PartnerID, &q.Customer.Name, &q.Customer.TaxID,
		&q.Currency, &q.OwnerID, &q.DepartmentID, &q.DepartmentPath, &q.State, &q.ValidUntil, &q.OrderID, &q.Totals.Net,
		&q.Totals.Tax, &q.Totals.Gross, &q.Version, &created, &updated, &lines)
	if err != nil {
		return q, err
	}
	q.CreatedAt, q.UpdatedAt = canon.Time(created), canon.Time(updated)
	return q, json.Unmarshal(lines, &q.Lines)
}

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	var created, updated time.Time
	var lines []byte
	var rate, rateDate, rateType, rateID *string
	var rateVersion *int64
	err := row.Scan(&o.ID, &o.Number, &o.LegalEntityID, &o.Customer.PartnerID, &o.Customer.Name, &o.Customer.TaxID,
		&o.Currency, &o.OwnerID, &o.DepartmentID, &o.DepartmentPath, &o.State, &o.BusinessDate, &rate, &rateDate, &rateType,
		&rateID, &rateVersion, &o.Revision, &o.QuotationID, &o.ConfirmationID, &o.Reason, &o.Totals.Net, &o.Totals.Tax,
		&o.Totals.Gross, &o.Version, &created, &updated, &lines)
	if err != nil {
		return o, err
	}
	if rate != nil {
		o.ExchangeRate = &Rate{Rate: *rate, Date: *rateDate, Type: *rateType, RecordID: *rateID, RecordVersion: *rateVersion}
	}
	o.CreatedAt, o.UpdatedAt = canon.Time(created), canon.Time(updated)
	return o, json.Unmarshal(lines, &o.Lines)
}

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	var created, updated time.Time
	var lines []byte
	err := row.Scan(&d.ID, &d.Number, &d.OrderID, &d.LegalEntityID, &d.State, &d.Reason, &d.BusinessDate, &d.OwnerID,
		&d.DepartmentID, &d.DepartmentPath, &d.Version, &created, &updated, &lines)
	if err != nil {
		return d, err
	}
	d.CreatedAt, d.UpdatedAt = canon.Time(created), canon.Time(updated)
	return d, json.Unmarshal(lines, &d.Lines)
}

func scanPriceList(row pgx.Row) (PriceList, error) {
	var p PriceList
	var at time.Time
	err := row.Scan(&p.ID, &p.LegalEntityID, &p.Currency, &p.Code, &p.Name, &p.State, &p.Version, &at)
	p.UpdatedAt = canon.Time(at)
	return p, err
}

func scanPriceItem(row pgx.Row) (PriceListItem, error) {
	var i PriceListItem
	var at time.Time
	err := row.Scan(&i.ID, &i.PriceListID, &i.ProductID, &i.Unit, &i.Price, &i.ValidFrom, &i.ValidTo, &i.Version, &at)
	i.UpdatedAt = canon.Time(at)
	return i, err
}

// errNone is a lookup that found no row.
var errNone = errors.New("no such record")

func one[T any](row pgx.Row, scan func(pgx.Row) (T, error)) (T, error) {
	v, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNone
	}
	return v, err
}

func getQuotation(ctx context.Context, tx db.Tx, id string, lock bool) (Quotation, error) {
	if lock {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM quotation WHERE id = $1::uuid FOR UPDATE`, id); err != nil {
			return Quotation{}, err
		}
	}
	return one(tx.QueryRow(ctx, `SELECT `+quotationCols+` FROM quotation q WHERE q.id = $1::uuid`, id), scanQuotation)
}

// getOrder reads an order; lock takes the order row's lock, the one every change of the order and its ledger takes.
func getOrder(ctx context.Context, tx db.Tx, id string, lock bool) (Order, error) {
	if lock {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM sales_order WHERE id = $1::uuid FOR UPDATE`, id); err != nil {
			return Order{}, err
		}
	}
	return one(tx.QueryRow(ctx, `SELECT `+orderCols+` FROM sales_order o WHERE o.id = $1::uuid`, id), scanOrder)
}

func getDelivery(ctx context.Context, tx db.Tx, id string, lock bool) (Delivery, error) {
	if lock {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM delivery WHERE id = $1::uuid FOR UPDATE`, id); err != nil {
			return Delivery{}, err
		}
	}
	return one(tx.QueryRow(ctx, `SELECT `+deliveryCols+` FROM delivery d WHERE d.id = $1::uuid`, id), scanDelivery)
}

// emitOrder publishes an order's state after its rows changed.
func (m *Module) emitOrder(ctx context.Context, tx db.Tx, id string) (Order, error) {
	o, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return o, err
	}
	return o, m.emit(ctx, tx, OrderChanged, o.ID, o.Version, o.LegalEntityID, o)
}
