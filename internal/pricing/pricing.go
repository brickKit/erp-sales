package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	masterdata "github.com/brickKit/erp-sales/v3/internal/masterdata"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
)

// Price lists and the price a line takes (docs/rules/pricing.md).

func leFacts(legalEntity string) sdkauthz.Facts {
	return sdkauthz.Facts{core.DimLegalEntity: sdkauthz.Value(legalEntity)}
}

func getPriceList(ctx context.Context, tx db.Tx, id string, lock bool) (core.PriceList, error) {
	q := `SELECT ` + core.PriceListCols + ` FROM price_list p WHERE p.id = $1::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	return core.One(tx.QueryRow(ctx, q, id), core.ScanPriceList)
}

func priceList(m *core.Base, ctx context.Context, id string) (core.PriceList, error) {
	if !core.UUIDForm.MatchString(id) {
		return core.PriceList{}, core.NotFound("price list")
	}
	var p core.PriceList
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		p, err = getPriceList(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, core.ErrNone) {
		return p, core.NotFound("price list")
	}
	return p, err
}

func CreatePriceList(m *core.Base, c *httpx.Context) error {
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
	case !core.UUIDForm.MatchString(in.LegalEntityID):
		return core.Invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	case !core.CurrencyForm.MatchString(in.Currency):
		return core.Invalid("currency", "currency is an ISO 4217 code")
	case !core.CodeForm.MatchString(in.Code):
		return core.Invalid("code", "code is upper case letters, digits, _ and -, at most 32")
	case strings.TrimSpace(in.Name) == "":
		return core.Invalid("name", "name is required")
	}
	ctx, actor, err := core.Check(c, core.OpPriceManage, leFacts(in.LegalEntityID))
	if err != nil {
		return err
	}
	if _, err := masterdata.LegalEntityOf(m, ctx, in.LegalEntityID); err != nil {
		return err
	}
	if _, err := masterdata.CurrencyScale(m, ctx, in.Currency); err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.RT.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		id := ids.New().String()
		tag, err := tx.Exec(ctx, `INSERT INTO price_list (id, legal_entity_id, currency, code, name) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (legal_entity_id, code) DO NOTHING`, id, in.LegalEntityID, in.Currency, in.Code, in.Name)
		if err != nil {
			return idem.Outcome{}, err
		}
		if tag.RowsAffected() == 0 {
			return idem.Outcome{}, core.ErrCode.New("price list code " + in.Code + " is taken in this legal entity")
		}
		p, err := getPriceList(ctx, tx, id, false)
		if err != nil {
			return idem.Outcome{}, err
		}
		if err := core.Emit(m, ctx, tx, core.PriceListChanged, p.ID, p.Version, p.LegalEntityID, p); err != nil {
			return idem.Outcome{}, err
		}
		if err := core.Change(m, ctx, tx, actor, core.OpPriceManage, core.PriceLists, p.ID, p); err != nil {
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
	p, err := priceList(m, ctx, res.ID)
	if err != nil {
		return err
	}
	return c.JSON(200, p)
}

func GetPriceList(m *core.Base, c *httpx.Context) error {
	p, err := priceList(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := core.Check(c, core.OpPriceRead, leFacts(p.LegalEntityID)); err != nil {
		return err
	}
	return c.JSON(200, p)
}

func UpdatePriceList(m *core.Base, c *httpx.Context) error {
	var in struct {
		Name    *string `json:"name"`
		State   *string `json:"state"`
		Version *int64  `json:"version"`
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.State != nil && *in.State != "active" && *in.State != "inactive" {
		return core.Invalid("state", "state is active or inactive")
	}
	cur, err := priceList(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := core.Check(c, core.OpPriceManage, leFacts(cur.LegalEntityID))
	if err != nil {
		return err
	}
	var out core.PriceList
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		p, err := getPriceList(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != p.Version {
			return core.ErrVersion.New(fmt.Sprintf("the price list is at version %d", p.Version))
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
		if err := core.Emit(m, ctx, tx, core.PriceListChanged, out.ID, out.Version, out.LegalEntityID, out); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, actor, core.OpPriceManage, core.PriceLists, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

// SetPrice adds a price valid from a date; the price of the same product and unit valid before it ends the day before.
func SetPrice(m *core.Base, c *httpx.Context) error {
	var in struct {
		ProductID string `json:"product_id"`
		Unit      string `json:"unit"`
		Price     string `json:"price"`
		ValidFrom string `json:"valid_from"`
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if !core.UUIDForm.MatchString(in.ProductID) || strings.TrimSpace(in.Unit) == "" || !core.DateForm.MatchString(in.ValidFrom) {
		return core.Invalid("product_id", "product_id, unit and valid_from (YYYY-MM-DD) are required")
	}
	price, err := core.Decimal("price", in.Price, core.PriceScale, false)
	if err != nil {
		return err
	}
	pl, err := priceList(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := core.Check(c, core.OpPriceManage, leFacts(pl.LegalEntityID))
	if err != nil {
		return err
	}
	var out core.PriceListItem
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
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
			ended, err := core.One(tx.QueryRow(ctx, `SELECT `+core.PriceItemCols+` FROM price_list_item i WHERE i.id = $1`, *prev), core.ScanPriceItem)
			if err != nil {
				return err
			}
			if err := core.Emit(m, ctx, tx, core.PriceListItemChange, ended.ID, ended.Version, pl.LegalEntityID, ended); err != nil {
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
		out, err = core.One(tx.QueryRow(ctx, `SELECT `+core.PriceItemCols+` FROM price_list_item i WHERE i.price_list_id = $1
			AND i.product_id = $2 AND i.unit = $3 AND i.valid_from = $4::date`, pl.ID, in.ProductID, in.Unit, in.ValidFrom), core.ScanPriceItem)
		if err != nil {
			return err
		}
		if err := core.Emit(m, ctx, tx, core.PriceListItemChange, out.ID, out.Version, pl.LegalEntityID, out); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, actor, core.OpPriceManage, core.PriceLists, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

var (
	priceListList = list.Spec{Operation: core.OpPriceRead, Alias: "p", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "code", Column: "code", Kind: list.Text, Sortable: true},
			{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true},
			{Field: "currency", Column: "currency", Kind: list.Text, Filterable: true}}}
	priceItemList = list.Spec{Operation: core.OpPriceRead, Alias: "i", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "product_id", Column: "product_id", Kind: list.UUID, Sortable: true, Filterable: true},
			{Field: "valid_from", Column: "valid_from", Kind: list.Date, Sortable: true, Filterable: true},
			{Field: "valid_to", Column: "valid_to", Kind: list.Date, Filterable: true},
			{Field: "price_list_id", Column: "price_list_id", Kind: list.UUID, Filterable: true}}}
)

func ListPriceLists(m *core.Base, c *httpx.Context) error {
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
	out, next, err := list.Page(ctx, m.RT.DB, q, "price_list", core.PriceListCols, core.ScanPriceList,
		func(p core.PriceList) ([]string, string) { return []string{p.Code}, p.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"price_lists": out, "next_cursor": next})
}

// ListPrices lists a price list's prices; the items carry the price list's legal entity for the dimension.
func ListPrices(m *core.Base, c *httpx.Context) error {
	pl, err := priceList(m, c.Ctx(), c.Param("id"))
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
		if !core.DateForm.MatchString(v) {
			return core.Invalid("on", "on is YYYY-MM-DD")
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
	out, next, err := list.Page(ctx, m.RT.DB, q, from, core.PriceItemCols, core.ScanPriceItem,
		func(i core.PriceListItem) ([]string, string) { return []string{i.ProductID, i.ValidFrom}, i.ID })
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

// PriceOf finds a line's unit price in the active price lists of the legal entity and currency valid on the date:
// the sales unit's own price, else the base unit's price times the factor (docs/rules/pricing.md).
func PriceOf(ctx context.Context, tx db.Tx, legalEntity, currency, product, unit, baseUnit, factor, date string) (string, *string, error) {
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
