package sales

import (
	"context"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/list"
)

// The lists of the user surface (protocol 14: cursor pages, newest first; the filters and keyword declared here).
// Every field is ordinary, so a row is listed whole or not at all; the legal entity, department and owner dimensions
// compile into the WHERE (the department on the copied department_path, see orgunits.go).

func docColumns(extra ...list.Column) []list.Column {
	return append([]list.Column{
		{Field: "created_at", Column: "created_at", Kind: list.Time, Sortable: true},
		{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true},
		{Field: "customer", Column: "customer_id", Kind: list.UUID, Filterable: true},
		{Field: "owner_id", Column: "owner_id", Kind: list.Text, Filterable: true},
		{Field: "department_id", Column: "department_id", Kind: list.UUID, Filterable: true},
		{Field: "state", Column: "state", Kind: list.Text, Filterable: true}}, extra...)
}

var (
	numberCustomer = []list.SearchColumn{{Name: "number_customer", Column: "search", Fields: []string{"number", "customer"}}}
	quotationList  = list.Spec{Operation: OpQuoteRead, Alias: "q", IDColumn: "id", IDKind: list.UUID,
		Columns: docColumns(), Search: numberCustomer}
	orderList = list.Spec{Operation: OpOrderRead, Alias: "o", IDColumn: "id", IDKind: list.UUID, Search: numberCustomer,
		Columns: docColumns(
			list.Column{Field: "to_deliver", Column: "to_deliver", Kind: list.Boolean, Filterable: true},
			list.Column{Field: "to_bill", Column: "to_bill", Kind: list.Boolean, Filterable: true})}
	deliveryList = list.Spec{Operation: OpDeliveryRead, Alias: "d", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "created_at", Column: "created_at", Kind: list.Time, Sortable: true},
			{Field: "order_id", Column: "order_id", Kind: list.UUID, Filterable: true},
			{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true},
			{Field: "state", Column: "state", Kind: list.Text, Filterable: true}}}
	newestFirst = []list.Order{{Field: "created_at", Desc: true}}
)

// filters turns the given query parameters (parameter → field) into equality filters, checking the ID ones.
func filters(c *httpx.Context, params map[string]string) ([]list.Filter, error) {
	var f []list.Filter
	for p, field := range params {
		v := c.Query(p)
		if v == "" {
			continue
		}
		if p != "state" && p != "to_deliver" && p != "to_bill" && !uuidForm.MatchString(v) {
			return nil, invalid(p, p+" is an ID")
		}
		if (p == "to_deliver" || p == "to_bill") && v != "true" && v != "false" {
			return nil, invalid(p, p+" is true or false")
		}
		f = append(f, list.Filter{Field: field, Op: "eq", Values: []string{v}})
	}
	return f, nil
}

func build(c *httpx.Context, spec list.Spec, f []list.Filter) (context.Context, *list.Query, error) {
	limit, err := list.ParseLimit(c.Query("limit"))
	if err != nil {
		return nil, nil, err
	}
	s, _ := sdkauthz.From(c.Ctx())
	return list.Build(c.Ctx(), s, spec, list.Request{Filters: f, Q: c.Query("q"), Limit: limit, Cursor: c.Query("cursor"),
		Sort: newestFirst})
}

var docParams = map[string]string{"legal_entity_id": "legal_entity_id", "customer_id": "customer", "owner_id": "owner_id",
	"department_id": "department_id", "state": "state"}

func (m *Module) listQuotations(c *httpx.Context) error {
	f, err := filters(c, docParams)
	if err != nil {
		return err
	}
	ctx, q, err := build(c, quotationList, f)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "quotation", quotationCols, scanQuotation,
		func(r Quotation) ([]string, string) { return []string{r.CreatedAt}, r.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"quotations": out, "next_cursor": next})
}

// orderFrom adds the two progress filters: goods left to deliver on a confirmed order, billable left on any line.
const orderFrom = `(SELECT s.*,
	s.state = 'confirmed' AND EXISTS (SELECT 1 FROM order_line l WHERE l.order_id = s.id AND l.kind = 'goods'
		AND l.delivered < l.quantity) AS to_deliver,
	EXISTS (SELECT 1 FROM order_line l WHERE l.order_id = s.id AND (l.claimed_qty + l.billed_qty < l.billable_qty
		OR l.claimed_amt + l.billed_amt < l.billable_amt)) AS to_bill
	FROM sales_order s)`

func (m *Module) listOrders(c *httpx.Context) error {
	params := map[string]string{"to_deliver": "to_deliver", "to_bill": "to_bill"}
	for k, v := range docParams {
		params[k] = v
	}
	f, err := filters(c, params)
	if err != nil {
		return err
	}
	ctx, q, err := build(c, orderList, f)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, orderFrom, orderCols, scanOrder,
		func(r Order) ([]string, string) { return []string{r.CreatedAt}, r.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"orders": out, "next_cursor": next})
}

func (m *Module) listDeliveries(c *httpx.Context) error {
	f, err := filters(c, map[string]string{"order_id": "order_id", "legal_entity_id": "legal_entity_id", "state": "state"})
	if err != nil {
		return err
	}
	ctx, q, err := build(c, deliveryList, f)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "delivery", deliveryCols, scanDelivery,
		func(r Delivery) ([]string, string) { return []string{r.CreatedAt}, r.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"deliveries": out, "next_cursor": next})
}
