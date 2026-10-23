// Command seed fills a running erp/sales with the seed data for people (be-assembly-standard's seed and test data
// conventions):
//
//   - access, by the authorization admin: the role maintain (the seed user, every legal entity, department and owner);
//     the sales accounts' planned scopes: test.sales.cn CN01 and the department SALES-EAST with what is below it,
//     test.sales.multi CN01 SALES-EAST and SALES-SOUTH, test.sales.es ES01 (every department); each may read and write
//     quotations, orders and deliveries and read price lists there;
//   - price lists: CN01 in CNY, EUR and USD, ES01 in EUR, US01 in USD, a price per base unit for every sellable product
//     of the legal entity (goods and services), valid from 2026-01-01;
//   - quotations in CN01: drafts, sent, cancelled and accepted ones (an accepted one is an order draft);
//   - orders in CN01 (CNY, and EUR for customers abroad), ES01 and US01, spread over departments and owners, in every
//     state: drafts; confirmed (stock and credit held and committed); refused at confirmation and back to draft with
//     the reason (no credit limit, over a small limit, not enough stock in BJ-01); cancelled drafts; confirmed then
//     cancelled; delivered in full; delivered in part. Only goods tracked by batch or not tracked are ordered, so the
//     deliveries need no serial numbers.
//
// Price lists are looked up by code. Quotations and orders are seeded once, into a legal entity that has no order yet;
// a run that broke off is finished by running it again within a day (the idempotency keys answer the requests already
// made). The legal entities, departments, customers, products, warehouses and credit limits come from mdm/org,
// mdm/partner, mdm/product, erp/inventory and erp/credit (their seeds run first).
//
//	SEED_IAM_URL (http://localhost:18200)    the identity member's development exchange
//	SEED_AUTHZ_URL (http://localhost:18223)  the authorization member's effect point
//	SEED_ADMIN (test.alice)                  the authorization admin (the install claim)
//	SEED_USER (test.bob), SEED_PASSWORD      the user that writes the data; the test accounts' password
//	ERP_SALES_URL (http://localhost:18084), MDM_ORG_URL (http://localhost:18083), MDM_PARTNER_URL (http://localhost:18225),
//	MDM_PRODUCT_URL (http://localhost:18082), ERP_INVENTORY_URL (http://localhost:18086), ERP_CREDIT_URL (http://localhost:18088)
package main

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/seed"
)

const prefix = "「本地测试」"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "seed erp/sales: "+format+"\n", a...)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

type client struct{ seed.Client }

func (c client) must(method, path string, body any, header ...string) map[string]any {
	out, err := c.Must(method, path, body, header...)
	check(err)
	return out
}

// pages reads every page of a list.
func (c client) pages(path, key string) []map[string]any {
	var out []map[string]any
	cursor := ""
	for {
		page := c.must("GET", path+"&limit=200&cursor="+url.QueryEscape(cursor), nil)
		for _, x := range page[key].([]any) {
			out = append(out, x.(map[string]any))
		}
		if cursor, _ = page["next_cursor"].(string); cursor == "" {
			return out
		}
	}
}

// segment grants an operation on a sales resource: in the legal entities (all when none) and the department subtrees
// (all when none), any owner. Price lists carry the legal entity only.
func segment(resource, op string, legalEntities, departments []string) sdkauthz.Segment {
	s := seed.Segment(resource, op)
	le := sdkauthz.Atom{Kind: "all"}
	if len(legalEntities) > 0 {
		le = sdkauthz.Atom{Kind: "in", Values: legalEntities}
	}
	s.Atoms = map[string]sdkauthz.Atom{"legal_entity": le}
	if resource == "erp.sales.price_list" {
		return s
	}
	dept := sdkauthz.Atom{Kind: "all"}
	if len(departments) > 0 {
		dept = sdkauthz.Atom{Kind: "tree"}
		for _, p := range departments {
			dept.Items = append(dept.Items, sdkauthz.TreeItem{Path: p, Mode: "subtree"})
		}
	}
	s.Atoms["department"], s.Atoms["owner"] = dept, sdkauthz.Atom{Kind: "all"}
	return s
}

// salesWork is what a salesperson does: quotations, orders and deliveries, reading price lists.
func salesWork(les, depts []string) []sdkauthz.Segment {
	var out []sdkauthz.Segment
	for _, op := range [][2]string{{"erp.sales.quotation", "erp.sales.quotation.read"}, {"erp.sales.quotation", "erp.sales.quotation.edit"},
		{"erp.sales.order", "erp.sales.order.read"}, {"erp.sales.order", "erp.sales.order.edit"},
		{"erp.sales.order", "erp.sales.order.confirm"}, {"erp.sales.order", "erp.sales.order.cancel"},
		{"erp.sales.delivery", "erp.sales.delivery.read"}, {"erp.sales.delivery", "erp.sales.delivery.create"},
		{"erp.sales.price_list", "erp.sales.price_list.read"}} {
		out = append(out, segment(op[0], op[1], les, depts))
	}
	return out
}

type product struct {
	id, code, kind, tracking, unit string
	sellable                      map[string]bool // legal entity → sellable
}

type customer struct{ id, currency string }

type world struct {
	les      map[string]string            // code → ID
	depts    map[string]map[string]string // legal entity code → department code → ID
	paths    map[string]map[string]string // legal entity code → department code → path
	products []product
	// active customer roles by legal entity code, and what erp/credit says of them in that currency
	customers map[string][]customer
	limits    map[string]string // partner/legal entity/currency → "none", "uncontrolled" or the amount
	whs       map[string]string // warehouse code → ID
}

func main() {
	base := env("ERP_SALES_URL", "http://localhost:18084")
	if !seed.Ready(base) {
		fail("erp/sales at %s is not ready", base)
	}
	pw := os.Getenv("SEED_PASSWORD")
	if pw == "" {
		fail("SEED_PASSWORD is not set (the test accounts' password in Casdoor)")
	}
	iam, user, admin := env("SEED_IAM_URL", "http://localhost:18200"), env("SEED_USER", "test.bob"), env("SEED_ADMIN", "test.alice")
	accounts, err := seed.Accounts(iam, pw, admin, user, "test.sales.cn", "test.sales.es", "test.sales.multi")
	check(err)
	t := accounts[user].Token
	w := read(t)
	a, err := seed.NewAuthz(env("SEED_AUTHZ_URL", "http://localhost:18223"), accounts[admin].Token, "erp/sales")
	check(err)
	maintain := salesWork(nil, nil)
	maintain = append(maintain, segment("erp.sales.price_list", "erp.sales.price_list.manage", nil, nil))
	cn := []string{w.les["CN01"]}
	_, err = a.GrantRoles([]seed.RoleSpec{
		{Key: "maintain", Name: prefix + "销售维护", To: []string{user}, Segments: maintain},
		{Key: "east", Name: prefix + "销售（CN01 华东区）", To: []string{"test.sales.cn"},
			Segments: salesWork(cn, []string{w.paths["CN01"]["SALES-EAST"]})},
		{Key: "east-south", Name: prefix + "销售（CN01 华东区与华南区）", To: []string{"test.sales.multi"},
			Segments: salesWork(cn, []string{w.paths["CN01"]["SALES-EAST"], w.paths["CN01"]["SALES-SOUTH"]})},
		{Key: "es01", Name: prefix + "销售（ES01）", To: []string{"test.sales.es"}, Segments: salesWork([]string{w.les["ES01"]}, nil)},
	}, accounts)
	check(err)

	c := client{seed.NewClient(base, t)}
	lists := priceLists(c, w)
	fmt.Printf("seed erp/sales: %d price lists\n", lists)
	owners := map[string]string{"east": accounts["test.sales.cn"].Principal, "south": accounts["test.sales.multi"].Principal,
		"es": accounts["test.sales.es"].Principal, "seed": accounts[user].Principal}
	for _, le := range []string{"CN01", "ES01", "US01"} {
		if len(c.must("GET", "/api/sales/orders?limit=1&legal_entity_id="+w.les[le], nil)["orders"].([]any)) > 0 {
			fmt.Printf("seed erp/sales: %s has orders already; quotations and orders are not seeded again\n", le)
			continue
		}
		n := orders(c, w, le, owners)
		fmt.Printf("seed erp/sales: %s: %d orders\n", le, n)
	}
}

// read gathers what the seed needs from the components it builds on.
func read(t string) world {
	w := world{les: map[string]string{}, depts: map[string]map[string]string{}, paths: map[string]map[string]string{},
		customers: map[string][]customer{}, limits: map[string]string{}, whs: map[string]string{}}
	org := client{seed.NewClient(env("MDM_ORG_URL", "http://localhost:18083"), t)}
	for _, e := range org.must("GET", "/api/org/legal-entities?limit=200", nil)["legal_entities"].([]any) {
		e := e.(map[string]any)
		w.les[e["code"].(string)] = e["id"].(string)
	}
	code := map[string]string{}
	for _, le := range []string{"CN01", "ES01", "US01"} {
		if w.les[le] == "" {
			fail("mdm/org has no legal entity %s: run its seed first", le)
		}
		code[w.les[le]] = le
		w.depts[le], w.paths[le] = map[string]string{}, map[string]string{}
		for _, u := range org.pages("/api/org/units?state=active&legal_entity_id="+w.les[le], "units") {
			w.depts[le][u["code"].(string)], w.paths[le][u["code"].(string)] = u["id"].(string), u["path"].(string)
		}
	}
	if w.paths["CN01"]["SALES-EAST"] == "" || w.paths["CN01"]["SALES-SOUTH"] == "" {
		fail("mdm/org has no CN01 SALES-EAST or SALES-SOUTH: run its seed first")
	}
	mdmProduct := client{seed.NewClient(env("MDM_PRODUCT_URL", "http://localhost:18082"), t)}
	for _, p := range mdmProduct.pages("/api/product/products?state=active", "products") {
		pr := product{id: p["id"].(string), code: p["code"].(string), kind: p["kind"].(string), tracking: p["tracking"].(string),
			unit: p["base_unit"].(string), sellable: map[string]bool{}}
		for _, av := range p["availability"].([]any) {
			av := av.(map[string]any)
			if le := code[av["legal_entity_id"].(string)]; le != "" && av["sellable"] == true {
				pr.sellable[le] = true
			}
		}
		if p["state"] == "active" && (pr.kind == "service" || p["stocked"] == true) {
			w.products = append(w.products, pr)
		}
	}
	sort.Slice(w.products, func(i, j int) bool { return w.products[i].code < w.products[j].code })
	partner := client{seed.NewClient(env("MDM_PARTNER_URL", "http://localhost:18225"), t)}
	partners := partner.pages("/api/partner/partners?state=active", "partners")
	sort.Slice(partners, func(i, j int) bool { return partners[i]["name"].(string) < partners[j]["name"].(string) })
	for _, p := range partners {
		for _, r := range partner.must("GET", "/api/partner/partners/"+p["id"].(string)+"/roles", nil)["roles"].([]any) {
			r := r.(map[string]any)
			le, _ := r["legal_entity_id"].(string)
			cur, _ := r["default_currency"].(string)
			if r["role"] == "customer" && r["state"] == "active" && code[le] != "" && cur != "" {
				w.customers[code[le]] = append(w.customers[code[le]], customer{p["id"].(string), cur})
			}
		}
	}
	credit := client{seed.NewClient(env("ERP_CREDIT_URL", "http://localhost:18088"), t)}
	for _, l := range credit.pages("/api/credit/limits?state=active", "limits") {
		v := "uncontrolled"
		if amount, ok := l["limit"].(string); ok {
			v = amount
		}
		w.limits[l["partner_id"].(string)+"/"+l["legal_entity_id"].(string)+"/"+l["currency"].(string)] = v
	}
	inv := client{seed.NewClient(env("ERP_INVENTORY_URL", "http://localhost:18086"), t)}
	for _, wh := range inv.pages("/api/inventory/warehouses?state=active", "warehouses") {
		w.whs[wh["code"].(string)] = wh["id"].(string)
	}
	for _, wh := range []string{"SH-01", "BJ-01", "MAD-01", "NYC-01"} {
		if w.whs[wh] == "" {
			fail("erp/inventory has no warehouse %s: run its seed first", wh)
		}
	}
	return w
}

// limitOf is what erp/credit says of a customer in a legal entity and currency.
func (w world) limitOf(le string, c customer) string {
	if v, ok := w.limits[c.id+"/"+w.les[le]+"/"+c.currency]; ok {
		return v
	}
	return "none"
}

// priceLists makes the price lists (by code) and their prices; it answers how many there are.
func priceLists(c client, w world) int {
	specs := [][3]string{{"CN01", "CNY", "STD-CNY"}, {"CN01", "EUR", "EXPORT-EUR"}, {"CN01", "USD", "EXPORT-USD"},
		{"ES01", "EUR", "STD-EUR"}, {"US01", "USD", "STD-USD"}}
	for _, s := range specs {
		le, cur, code := s[0], s[1], s[2]
		var id string
		for _, pl := range c.pages("/api/sales/price-lists?legal_entity_id="+w.les[le], "price_lists") {
			if pl["code"] == code {
				id = pl["id"].(string)
			}
		}
		if id == "" {
			st, out, err := c.UntilGranted(30*time.Second, "POST", "/api/sales/price-lists", map[string]any{
				"legal_entity_id": w.les[le], "currency": cur, "code": code, "name": prefix + le + " " + cur + " 价格表"},
				"Idempotency-Key", "seed:erp/sales:price-list:"+code)
			check(err)
			if st != 200 {
				fail("price list %s: %d %v", code, st, out)
			}
			id = out["id"].(string)
		}
		have := map[string]bool{}
		for _, it := range c.pages("/api/sales/price-lists/"+id+"/items?on=2026-01-01", "items") {
			have[it["product_id"].(string)] = true
		}
		for j, p := range w.products {
			if !p.sellable[le] || have[p.id] {
				continue
			}
			price := fmt.Sprintf("%d.%02d", 20+(j%30)*3, (j*7)%100)
			if p.kind == "service" {
				price = "200"
			}
			if cur != "CNY" {
				price = fmt.Sprintf("%d.%02d", 3+(j%30)/2, (j*3)%100) // roughly a seventh, abroad
			}
			c.must("POST", "/api/sales/price-lists/"+id+"/items", map[string]any{"product_id": p.id, "unit": p.unit,
				"price": price, "valid_from": "2026-01-01"})
		}
	}
	return len(specs)
}

// orders seeds one legal entity's quotations and orders; it answers how many orders it made.
func orders(c client, w world, le string, owners map[string]string) int {
	var goods, services []product
	for _, p := range w.products {
		if !p.sellable[le] {
			continue
		}
		switch {
		case p.kind == "service":
			services = append(services, p)
		case p.tracking != "serial":
			goods = append(goods, p)
		}
	}
	if len(goods) == 0 {
		fail("%s has no sellable goods: run mdm/product's seed first", le)
	}
	local := map[string]string{"CN01": "CNY", "ES01": "EUR", "US01": "USD"}[le]
	wh := map[string]string{"CN01": "SH-01", "ES01": "MAD-01", "US01": "NYC-01"}[le]
	var good, noLimit, small []customer // confirmable, refused for no limit, a small limit to exceed
	var foreign []customer
	for _, cu := range w.customers[le] {
		l := w.limitOf(le, cu)
		switch {
		case cu.currency != local:
			if l == "uncontrolled" || l == "1000000" {
				foreign = append(foreign, cu)
			}
		case l == "none":
			noLimit = append(noLimit, cu)
		case l == "20000":
			small = append(small, cu)
		case l == "uncontrolled" || l == "1000000" || (le == "ES01"):
			good = append(good, cu)
		}
	}
	if len(good) == 0 {
		fail("%s has no customer with a large or no credit limit: run erp/credit's seed first", le)
	}
	type slot struct{ dept, owner string }
	var slots []slot
	switch le {
	case "CN01":
		for _, d := range []string{"SALES-EAST", "SALES-EAST-KA", "SALES-EAST-SMB"} {
			slots = append(slots, slot{d, owners["east"]})
		}
		for _, d := range []string{"SALES-SOUTH", "SALES-SOUTH-KA"} {
			slots = append(slots, slot{d, owners["south"]})
		}
		for _, d := range []string{"SALES-WEST", "SALES-NORTH", "OPS", ""} {
			slots = append(slots, slot{d, owners["seed"]})
		}
	case "ES01":
		slots = []slot{{"", owners["es"]}}
		for code := range w.depts["ES01"] {
			if len(slots) < 3 {
				slots = append(slots, slot{code, owners["es"]})
			}
		}
	default:
		slots = []slot{{"", owners["seed"]}}
	}
	total := map[string]int{"CN01": 160, "ES01": 30, "US01": 20}[le]
	key := func(kind string, n int) string { return fmt.Sprintf("seed:erp/sales:%s:%s:%d", le, kind, n) }
	head := func(n int, cu customer) map[string]any {
		s := slots[n%len(slots)]
		body := map[string]any{"legal_entity_id": w.les[le], "customer_id": cu.id, "currency": cu.currency, "owner_id": s.owner}
		if s.dept != "" && w.depts[le][s.dept] != "" {
			body["department_id"] = w.depts[le][s.dept]
		}
		return body
	}
	lines := func(n int, qty int, whCode string) []map[string]any {
		g := goods[n%len(goods)]
		out := []map[string]any{{"product_id": g.id, "quantity": fmt.Sprint(qty), "unit": g.unit, "warehouse_id": w.whs[whCode],
			"tax_rate": "0.13"}}
		if n%3 == 0 {
			g2 := goods[(n*7+3)%len(goods)]
			if g2.id != g.id {
				out = append(out, map[string]any{"product_id": g2.id, "quantity": "2", "unit": g2.unit,
					"warehouse_id": w.whs[whCode], "tax_rate": "0.13"})
			}
		}
		if n%4 == 1 && len(services) > 0 {
			s := services[n%len(services)]
			out = append(out, map[string]any{"product_id": s.id, "quantity": "2", "unit": s.unit, "tax_rate": "0.06"})
		}
		return out
	}
	post := func(path, k string, body any) map[string]any {
		st, out, err := c.Do("POST", path, body, "Idempotency-Key", k)
		check(err)
		if st != 200 {
			fail("POST %s (%s): %d %v", path, k, st, out)
		}
		return out
	}

	// quotations (CN01 only): drafts, sent, cancelled, accepted
	if le == "CN01" {
		for n := 0; n < 20; n++ {
			body := head(n, good[n%len(good)])
			body["valid_until"] = "2099-12-31"
			body["lines"] = lines(n, 1+n%3, wh)
			q := post("/api/sales/quotations", key("quotation", n), body)
			id := q["id"].(string)
			switch n % 4 {
			case 1:
				post("/api/sales/quotations/"+id+"/send", key("quotation-send", n), nil)
			case 2:
				post("/api/sales/quotations/"+id+"/cancel", key("quotation-cancel", n), nil)
			case 3:
				post("/api/sales/quotations/"+id+"/send", key("quotation-send", n), nil)
				post("/api/sales/quotations/"+id+"/accept", key("quotation-accept", n), nil)
			}
		}
	}

	made := 0
	for n := 0; n < total; n++ {
		state := n % 10
		cu := good[n%len(good)]
		whCode := wh
		qty := 1 + n%4
		switch {
		case state == 2 && le != "ES01" && len(noLimit) > 0: // refused: no credit limit under a block policy
			cu = noLimit[n%len(noLimit)]
		case state == 2 && len(small) > 0: // refused: over a 20,000 limit
			cu, qty = small[n%len(small)], 900
		case state == 2 && le == "CN01": // refused: not enough stock in Beijing (TEA-00-TIN has one there)
			whCode = "BJ-01"
		}
		if le == "CN01" && state == 8 && len(foreign) > 0 { // a confirmed order in the customer's currency (EUR, USD)
			cu = foreign[n%len(foreign)]
		}
		body := head(n, cu)
		body["lines"] = lines(n, qty, whCode)
		if state == 2 && whCode == "BJ-01" {
			for _, g := range goods {
				if g.code == "TEA-00-TIN" {
					body["lines"] = []map[string]any{{"product_id": g.id, "quantity": "2", "unit": g.unit,
						"warehouse_id": w.whs["BJ-01"], "tax_rate": "0.13"}}
				}
			}
		}
		o := post("/api/sales/orders", key("order", n), body)
		made++
		id := o["id"].(string)
		if state == 0 || state == 1 {
			continue // drafts
		}
		if state == 3 {
			post("/api/sales/orders/"+id+"/cancel", key("order-cancel", n), nil)
			continue
		}
		ans := post("/api/sales/orders/"+id+"/confirm", key("order-confirm", n), nil)
		if ans["outcome"] != "confirmed" {
			if state != 2 {
				fmt.Printf("seed erp/sales: order %d of %s was not confirmed: %v %v\n", n, le, ans["outcome"], ans["reason"])
			}
			continue
		}
		order := ans["order"].(map[string]any)
		switch state {
		case 4: // confirmed, then cancelled
			post("/api/sales/orders/"+id+"/cancel", key("order-cancel", n), nil)
		case 5, 6, 7: // delivered in full, or the first line in part
			var dl []map[string]any
			for _, l := range order["lines"].([]any) {
				l := l.(map[string]any)
				if l["kind"] != "goods" {
					continue
				}
				q := l["quantity"].(string)
				if state == 7 && len(dl) == 0 && q != "1" {
					q = "1"
				}
				dl = append(dl, map[string]any{"order_line_id": l["id"], "quantity": q})
				if state == 7 {
					break
				}
			}
			if len(dl) > 0 {
				post("/api/sales/deliveries", key("delivery", n), map[string]any{"order_id": id, "lines": dl})
			}
		}
	}
	return made
}
