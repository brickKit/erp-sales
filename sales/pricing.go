package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
)

// Price lists and the price a line takes (docs/rules/pricing.md).

func leFacts(legalEntity string) sdkauthz.Facts {
	return sdkauthz.Facts{DimLegalEntity: sdkauthz.Value(legalEntity)}
}

func getPriceList(ctx context.Context, tx db.Tx, id string, lock bool) (PriceList, error) {
	q := `SELECT ` + priceListCols + ` FROM price_list p WHERE p.id = $1::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	return one(tx.QueryRow(ctx, q, id), scanPriceList)
}

func (m *Module) priceList(ctx context.Context, id string) (PriceList, error) {
	if !uuidForm.MatchString(id) {
		return PriceList{}, notFound("price list")
	}
	var p PriceList
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		p, err = getPriceList(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, errNone) {
		return p, notFound("price list")
	}
	return p, err
}

func (m *Module) createPriceList(c *httpx.Context) error {
	var in struct {
		LegalEntityID string `json:"legal_entity_id"`
		Currency      string `json:"currency"`
		Code          string `json:"code"`
		Name          string `json:"name"`
	}
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	switch {
	case !uuidForm.MatchString(in.LegalEntityID):
		return invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	case !currencyForm.MatchString(in.Currency):
		return invalid("currency", "currency is an ISO 4217 code")
	case !codeForm.MatchString(in.Code):
		return invalid("code", "code is upper case letters, digits, _ and -, at most 32")
	case strings.TrimSpace(in.Name) == "":
		return invalid("name", "name is required")
	}
	ctx, actor, err := check(c, OpPriceManage, leFacts(in.LegalEntityID))
	if err != nil {
		return err
	}
	if _, err := m.legalEntityOf(ctx, in.LegalEntityID); err != nil {
		return err
	}
	if _, err := m.currencyScale(ctx, in.Currency); err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.rt.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		id := ids.New().String()
		tag, err := tx.Exec(ctx, `INSERT INTO price_list (id, legal_entity_id, currency, code, name) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (legal_entity_id, code) DO NOTHING`, id, in.LegalEntityID, in.Currency, in.Code, in.Name)
		if err != nil {
			return idem.Outcome{}, err
		}
		if tag.RowsAffected() == 0 {
			return idem.Outcome{}, errCode.New("price list code " + in.Code + " is taken in this legal entity")
		}
		p, err := getPriceList(ctx, tx, id, false)
		if err != nil {
			return idem.Outcome{}, err
		}
		if err := m.emit(ctx, tx, PriceListChanged, p.ID, p.Version, p.LegalEntityID, p); err != nil {
			return idem.Outcome{}, err
		}
		if err := m.change(ctx, tx, actor, OpPriceManage, PriceLists, p.ID, p); err != nil {
			return idem.Outcome{}, err
		}
		res, _ := json.Marshal(map[string]string{"id": p.ID})
		return idem.Outcome{Status: 200, Result: res}, nil
	})
	if err != nil {
		return err
	}
	var res struct{ ID string }
	_ = json.Unmarshal(out.Result, &res)
	p, err := m.priceList(ctx, res.ID)
	if err != nil {
		return err
	}
	return c.JSON(200, p)
}

func (m *Module) getPriceList(c *httpx.Context) error {
	p, err := m.priceList(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := check(c, OpPriceRead, leFacts(p.LegalEntityID)); err != nil {
		return err
	}
	return c.JSON(200, p)
}

func (m *Module) updatePriceList(c *httpx.Context) error {
	var in struct {
		Name    *string `json:"name"`
		State   *string `json:"state"`
		Version *int64  `json:"version"`
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.State != nil && *in.State != "active" && *in.State != "inactive" {
		return invalid("state", "state is active or inactive")
	}
	cur, err := m.priceList(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := check(c, OpPriceManage, leFacts(cur.LegalEntityID))
	if err != nil {
		return err
	}
	var out PriceList
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		p, err := getPriceList(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != p.Version {
			return errVersion.New(fmt.Sprintf("the price list is at version %d", p.Version))
		}
		next := p
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.State != nil {
			next.State = *in.State
		}
		if next == p {
			out = p
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE price_list SET name = $2, state = $3, version = version + 1, updated_at = now()
			WHERE id = $1`, p.ID, next.Name, next.State); err != nil {
			return err
		}
		if out, err = getPriceList(ctx, tx, p.ID, false); err != nil {
			return err
		}
		if err := m.emit(ctx, tx, PriceListChanged, out.ID, out.Version, out.LegalEntityID, out); err != nil {
			return err
		}
		return m.change(ctx, tx, actor, OpPriceManage, PriceLists, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

// setPrice adds a price valid from a date; the price of the same product and unit valid before it ends the day before.
func (m *Module) setPrice(c *httpx.Context) error {
	var in struct {
		ProductID string `json:"product_id"`
		Unit      string `json:"unit"`
		Price     string `json:"price"`
		ValidFrom string `json:"valid_from"`
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if !uuidForm.MatchString(in.ProductID) || strings.TrimSpace(in.Unit) == "" || !dateForm.MatchString(in.ValidFrom) {
		return invalid("product_id", "product_id, unit and valid_from (YYYY-MM-DD) are required")
	}
	price, err := decimal("price", in.Price, PriceScale, false)
	if err != nil {
		return err
	}
	pl, err := m.priceList(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := check(c, OpPriceManage, leFacts(pl.LegalEntityID))
	if err != nil {
		return err
	}
	var out PriceListItem
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := getPriceList(ctx, tx, pl.ID, true); err != nil {
			return err
		}
		var prev *string
		_ = tx.QueryRow(ctx, `SELECT id::text FROM price_list_item WHERE price_list_id = $1 AND product_id = $2 AND unit = $3
			AND valid_from < $4::date AND (valid_to IS NULL OR valid_to >= $4::date) ORDER BY valid_from DESC LIMIT 1`,
			pl.ID, in.ProductID, in.Unit, in.ValidFrom).Scan(&prev)
		if prev != nil {
			if _, err := tx.Exec(ctx, `UPDATE price_list_item SET valid_to = $2::date - 1, version = version + 1, updated_at = now()
				WHERE id = $1`, *prev, in.ValidFrom); err != nil {
				return err
			}
			ended, err := one(tx.QueryRow(ctx, `SELECT `+priceItemCols+` FROM price_list_item i WHERE i.id = $1`, *prev), scanPriceItem)
			if err != nil {
				return err
			}
			if err := m.emit(ctx, tx, PriceListItemChange, ended.ID, ended.Version, pl.LegalEntityID, ended); err != nil {
				return err
			}
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO price_list_item (id, price_list_id, product_id, unit, price, valid_from)
			VALUES ($1, $2, $3, $4, $5::numeric, $6::date) ON CONFLICT (price_list_id, product_id, unit, valid_from) DO UPDATE
			SET price = excluded.price, version = price_list_item.version + 1, updated_at = now()`,
			id, pl.ID, in.ProductID, in.Unit, price, in.ValidFrom); err != nil {
			return err
		}
		var err error
		out, err = one(tx.QueryRow(ctx, `SELECT `+priceItemCols+` FROM price_list_item i WHERE i.price_list_id = $1
			AND i.product_id = $2 AND i.unit = $3 AND i.valid_from = $4::date`, pl.ID, in.ProductID, in.Unit, in.ValidFrom), scanPriceItem)
		if err != nil {
			return err
		}
		if err := m.emit(ctx, tx, PriceListItemChange, out.ID, out.Version, pl.LegalEntityID, out); err != nil {
			return err
		}
		return m.change(ctx, tx, actor, OpPriceManage, PriceLists, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

var (
	priceListList = list.Spec{Operation: OpPriceRead, Alias: "p", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "code", Column: "code", Kind: list.Text, Sortable: true},
			{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true},
			{Field: "currency", Column: "currency", Kind: list.Text, Filterable: true}}}
	priceItemList = list.Spec{Operation: OpPriceRead, Alias: "i", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "product_id", Column: "product_id", Kind: list.UUID, Sortable: true, Filterable: true},
			{Field: "valid_from", Column: "valid_from", Kind: list.Date, Sortable: true, Filterable: true},
			{Field: "valid_to", Column: "valid_to", Kind: list.Date, Filterable: true},
			{Field: "price_list_id", Column: "price_list_id", Kind: list.UUID, Filterable: true}}}
)

func (m *Module) listPriceLists(c *httpx.Context) error {
	limit, err := list.ParseLimit(c.Query("limit"))
	if err != nil {
		return err
	}
	var f []list.Filter
	for _, k := range []string{"legal_entity_id", "currency"} {
		if v := c.Query(k); v != "" {
			f = append(f, list.Filter{Field: k, Op: "eq", Values: []string{v}})
		}
	}
	s, _ := sdkauthz.From(c.Ctx())
	ctx, q, err := list.Build(c.Ctx(), s, priceListList, list.Request{Filters: f, Limit: limit, Cursor: c.Query("cursor"),
		Sort: []list.Order{{Field: "code"}}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "price_list", priceListCols, scanPriceList,
		func(p PriceList) ([]string, string) { return []string{p.Code}, p.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"price_lists": out, "next_cursor": next})
}

// listPrices lists a price list's prices; the items carry the price list's legal entity for the dimension.
func (m *Module) listPrices(c *httpx.Context) error {
	pl, err := m.priceList(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	limit, err := list.ParseLimit(c.Query("limit"))
	if err != nil {
		return err
	}
	f := []list.Filter{{Field: "price_list_id", Op: "eq", Values: []string{pl.ID}}}
	if v := c.Query("product_id"); v != "" {
		f = append(f, list.Filter{Field: "product_id", Op: "eq", Values: []string{v}})
	}
	if v := c.Query("on"); v != "" {
		if !dateForm.MatchString(v) {
			return invalid("on", "on is YYYY-MM-DD")
		}
		f = append(f, list.Filter{Field: "valid_from", Op: "lte", Values: []string{v}})
	}
	s, _ := sdkauthz.From(c.Ctx())
	ctx, q, err := list.Build(c.Ctx(), s, priceItemList, list.Request{Filters: f, Limit: limit, Cursor: c.Query("cursor"),
		Sort: []list.Order{{Field: "product_id"}, {Field: "valid_from"}}})
	if err != nil {
		return err
	}
	from := "(SELECT i.*, p.legal_entity_id FROM price_list_item i JOIN price_list p ON p.id = i.price_list_id)"
	out, next, err := list.Page(ctx, m.rt.DB, q, from, priceItemCols, scanPriceItem,
		func(i PriceListItem) ([]string, string) { return []string{i.ProductID, i.ValidFrom}, i.ID })
	if err != nil {
		return err
	}
	if v := c.Query("on"); v != "" {
		kept := out[:0]
		for _, i := range out {
			if i.ValidTo == nil || *i.ValidTo >= v {
				kept = append(kept, i)
			}
		}
		out = kept
	}
	return c.JSON(200, map[string]any{"items": out, "next_cursor": next})
}

// priceOf finds a line's unit price in the active price lists of the legal entity and currency valid on the date:
// the sales unit's own price, else the base unit's price times the factor (docs/rules/pricing.md).
func priceOf(ctx context.Context, tx db.Tx, legalEntity, currency, product, unit, baseUnit, factor, date string) (string, *string, error) {
	q := `SELECT i.id::text, trim_scale(i.price)::text FROM price_list_item i JOIN price_list p ON p.id = i.price_list_id
		WHERE p.legal_entity_id = $1 AND p.currency = $2 AND p.state = 'active' AND i.product_id = $3 AND i.unit = $4
		AND i.valid_from <= $5::date AND (i.valid_to IS NULL OR i.valid_to >= $5::date)
		ORDER BY i.valid_from DESC, p.code LIMIT 1`
	var id, price string
	err := tx.QueryRow(ctx, q, legalEntity, currency, product, unit, date).Scan(&id, &price)
	if err == nil {
		return price, &id, nil
	}
	if unit != baseUnit {
		if err := tx.QueryRow(ctx, q, legalEntity, currency, product, baseUnit, date).Scan(&id, &price); err == nil {
			var p string
			if err := tx.QueryRow(ctx, `SELECT trim_scale(round($1::numeric * $2::numeric, 6))::text`, price, factor).Scan(&p); err != nil {
				return "", nil, err
			}
			return p, &id, nil
		}
	}
	return "", nil, nil
}
