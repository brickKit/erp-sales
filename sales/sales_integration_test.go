//go:build integration

package sales

// The sales component against PostgreSQL and the bus over its real surfaces (L2). The master data components and the
// two participants (erp/inventory, erp/credit) are stand-ins that speak their published contracts over mTLS
// (standins_integration_test.go); the billing source is called as erp/billing. Needs BE_SDK_TEST_PG_ADMIN and
// BE_SDK_TEST_NATS_URL, as be-sdk-go's integration tests. The member listens on 28084 (user) and 9094 (system); the
// stand-ins on 19102-19107.
// Run: go test -tags integration ./sales/
//
// What it proves (protocol 04's RS, protocol 05's BL): RS02, RS03, RS04, RS05, RS07, RS08, RS09, RS14, RS19 (the
// delivery is the consumption ID); BL01, BL02, BL05/BL26, BL24; the pricing rules; the department dimension following
// a department's move. RS06 (a crash between the last commit and the local confirmation) needs L3. The steps are in
// flows_integration_test.go, in the order they run.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/config"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/events/bus"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	creditv3 "github.com/brickKit/erp-credit/contract/v3/gen/go/be/erp/credit/v3"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	currencyv3 "github.com/brickKit/mdm-currency/contract/v3/gen/go/be/mdm/currency/v3"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	partnerv3 "github.com/brickKit/mdm-partner/contract/v3/gen/go/be/mdm/partner/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	itPort      = "28084"
	itBase      = "http://localhost:" + itPort
	itSystem    = "localhost:9094"
	itOrg       = "localhost:19102"
	itPartner   = "localhost:19103"
	itProduct   = "localhost:19104"
	itCurrency  = "localhost:19105"
	itInventory = "localhost:19106"
	itCredit    = "localhost:19107"
	itSchema    = "erp_sales_it"
)

// fixture is the running member, its stand-ins and the world's IDs, shared by the steps.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	w     *sectest.World
	admin *pgx.Conn
	alice client
	org   *orgStandIn
	inv   *inventoryStandIn
	cred  *creditStandIn
	tr    events.Transport
	year  string

	le, d1, d2, cust, blocked, screw, install, bolt, wh string
	// what one step leaves for the next: orders by letter, their confirmation processes
	orders map[string]map[string]any
	procs  map[string]map[string]any
}

func TestSalesEndToEnd(t *testing.T) {
	fx := start(t)
	fx.pricingAndQuotations()
	fx.confirmations()
	fx.deliveries()
	fx.billingSource()
	fx.lists()
	fx.departments()
	fx.outbox()
}

// start makes the world, the stand-ins and mdm/org's stream, then migrates and runs the member.
func start(t *testing.T) *fixture {
	adminURL, natsURL := os.Getenv("BE_SDK_TEST_PG_ADMIN"), os.Getenv("BE_SDK_TEST_NATS_URL")
	if adminURL == "" || natsURL == "" {
		t.Fatal("BE_SDK_TEST_PG_ADMIN and BE_SDK_TEST_NATS_URL are needed")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close(ctx) })
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+itSchema+" CASCADE; CREATE SCHEMA "+itSchema)
	require.NoError(t, err)
	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	pw, _ := u.User.Password()
	w := sectest.New(t)
	w.GrantAll(sectest.Alice, Definitions())

	// one legal entity in Shanghai (code SH, base CNY), two departments, two customers, three products
	fx := &fixture{t: t, ctx: ctx, w: w, admin: admin, le: ids.New().String(), d1: ids.New().String(),
		d2: ids.New().String(), cust: ids.New().String(), blocked: ids.New().String(), screw: ids.New().String(),
		install: ids.New().String(), bolt: ids.New().String(), wh: ids.New().String(),
		orders: map[string]map[string]any{}, procs: map[string]map[string]any{}}
	fx.org = &orgStandIn{les: map[string]map[string]any{fx.le: {"id": fx.le, "code": "SH", "state": "active",
		"timezone": "Asia/Shanghai", "base_currency": "CNY", "version": int64(1)}},
		units: map[string]map[string]any{fx.d1: fx.unit(fx.d1, fx.le+"/"+fx.d1, 1), fx.d2: fx.unit(fx.d2, fx.le+"/"+fx.d2, 1)}}
	fx.inv = &inventoryStandIn{participant: newParticipant("insufficient_stock"), issued: map[string]*inventoryv3.IssueRequest{}}
	fx.cred = &creditStandIn{participant: newParticipant("over_limit")}
	portFree(t, itPort, itSystem, itOrg, itPartner, itProduct, itCurrency, itInventory, itCredit)
	serve(t, itOrg, w, Org, func(s *grpc.Server) { orgv3.RegisterSourceServiceServer(s, fx.org) })
	serve(t, itPartner, w, Partner, func(s *grpc.Server) {
		p := &partnerStandIn{names: map[string]string{fx.cust: "上海客户", fx.blocked: "停用客户"},
			blocked: map[string]bool{fx.blocked: true}}
		partnerv3.RegisterSourceServiceServer(s, p)
		partnerv3.RegisterPartnerServiceServer(s, p)
	})
	serve(t, itProduct, w, Product, func(s *grpc.Server) {
		p := &productStandIn{known: map[string]productInfoIT{
			fx.screw:   {Code: "SCREW", Name: "螺丝", BaseUnit: "EA", Stocked: true, Box: "12"},
			fx.install: {Code: "INSTALL", Name: "安装", BaseUnit: "HUR"},
			fx.bolt:    {Code: "BOLT", Name: "螺栓", BaseUnit: "EA", Stocked: true}}}
		productv3.RegisterSourceServiceServer(s, p)
		productv3.RegisterProductServiceServer(s, p)
	})
	serve(t, itCurrency, w, Currency, func(s *grpc.Server) {
		currencyv3.RegisterSourceServiceServer(s, currencyStandIn{})
		currencyv3.RegisterRateServiceServer(s, currencyStandIn{})
	})
	serve(t, itInventory, w, Inventory, func(s *grpc.Server) {
		inventoryv3.RegisterReservationServiceServer(s, fx.inv)
		inventoryv3.RegisterStockServiceServer(s, fx.inv)
	})
	serve(t, itCredit, w, Credit, func(s *grpc.Server) { creditv3.RegisterReservationServiceServer(s, fx.cred) })

	// mdm/org's stream exists before the subscription starts (mdm/org's own forwarder makes it in a deployment)
	fx.tr, err = bus.Open(ctx, natsURL, "sales-it-org", slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { fx.tr.Close() })
	require.NoError(t, fx.tr.EnsureStream(ctx, orgStream))

	env := w.Env(t, ID)
	for k, v := range map[string]string{"COMPONENT_ID": ID, "COMPONENT_VERSION": Version, "PORT": itPort,
		"STOP_DRAIN_DELAY": "50ms", "PG_HOST": u.Hostname(), "PG_PORT": u.Port(), "PG_DATABASE": strings.TrimPrefix(u.Path, "/"),
		"PG_USER": u.User.Username(), "PG_PASSWORD": pw, "PG_SCHEMA": itSchema, "PG_OWNER_USER": u.User.Username(),
		"PG_OWNER_PASSWORD": pw, "BUS_URL": natsURL, "IMPLEMENTATION_INSTANCE": ids.New().String(),
		"MDM_ORG_GRPC_ENDPOINT": "http://" + itOrg, "MDM_PARTNER_GRPC_ENDPOINT": "http://" + itPartner,
		"MDM_PRODUCT_GRPC_ENDPOINT": "http://" + itProduct, "MDM_CURRENCY_GRPC_ENDPOINT": "http://" + itCurrency,
		"ERP_INVENTORY_GRPC_ENDPOINT": "http://" + itInventory, "ERP_CREDIT_GRPC_ENDPOINT": "http://" + itCredit} {
		env[k] = v
	}
	var logs bytes.Buffer
	var logMu sync.Mutex
	t.Cleanup(func() {
		if t.Failed() {
			logMu.Lock()
			t.Log(logs.String())
			logMu.Unlock()
		}
	})
	out := &lockedWriter{w: &logs, mu: &logMu}
	opts := launcher.Options{Stdout: out, Stderr: out,
		Env: config.Env{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Read: os.ReadFile}}
	migrate := opts
	migrate.Args = []string{"migrate"}
	require.Equal(t, launcher.ExitOK, launcher.Run(ctx, Definition(), migrate))
	sctx, stop := context.WithCancel(ctx)
	done := make(chan int, 1)
	go func() { done <- launcher.Run(sctx, Definition(), opts) }()
	t.Cleanup(func() { stop(); <-done })
	require.Eventually(t, func() bool {
		r, err := http.Get(itBase + "/readyz")
		if err == nil {
			r.Body.Close()
		}
		return err == nil && r.StatusCode == 200
	}, 20*time.Second, 100*time.Millisecond)
	fx.alice = client{t, w.Bearer(sectest.Alice)}
	today, err := bizdate.Today("Asia/Shanghai")
	require.NoError(t, err)
	fx.year = today[:4]
	return fx
}

var orgStream = events.Stream{Name: OrgStream, Partitions: 4}

// --- helpers ---

type client struct {
	t     *testing.T
	token string
}

// do calls the user surface; a POST carries a new Idempotency-Key unless key gives one.
func (c client) do(method, path string, body any, key ...string) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, itBase+path, r)
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")
	if method == "POST" {
		k := ids.New().String()
		if len(key) > 0 {
			k = key[0]
		}
		req.Header.Set("Idempotency-Key", k)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func serve(t *testing.T, addr string, w *sectest.World, as string, register func(*grpc.Server)) {
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(w.TLS(t, as).ServerConfig())))
	register(srv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
}

// at reads a value along a path of keys and indexes from a decoded JSON object.
func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func (fx *fixture) unit(id, path string, version int64) map[string]any {
	return map[string]any{"id": id, "legal_entity_id": fx.le, "parent_id": nil, "code": "D" + id[:4], "name": "部门",
		"path": path, "state": "active", "version": version, "updated_at": "2026-10-01T00:00:00.000000Z"}
}

// goods is a line of qty boxes of screws (12 EA a box, 10 CNY an EA on the price list); service is 1.5 hours at 100.
func (fx *fixture) goods(qty string) map[string]any {
	return map[string]any{"product_id": fx.screw, "quantity": qty, "unit": "BX", "warehouse_id": fx.wh, "tax_rate": "0.13"}
}

func (fx *fixture) service() map[string]any {
	return map[string]any{"product_id": fx.install, "quantity": "1.5", "unit": "HUR", "unit_price": "100", "tax_rate": "0.06"}
}

// newOrder creates an order draft of the legal entity and customer with the given fields.
func (fx *fixture) newOrder(body map[string]any) map[string]any {
	fx.t.Helper()
	in := map[string]any{"legal_entity_id": fx.le, "customer_id": fx.cust, "currency": "CNY"}
	for k, v := range body {
		in[k] = v
	}
	code, o := fx.alice.do("POST", "/api/sales/orders", in)
	require.Equal(fx.t, 200, code, o)
	return o
}

func (fx *fixture) order(lines ...map[string]any) map[string]any {
	fx.t.Helper()
	return fx.newOrder(map[string]any{"lines": lines})
}

func (fx *fixture) confirm(o map[string]any, key ...string) map[string]any {
	fx.t.Helper()
	code, a := fx.alice.do("POST", "/api/sales/orders/"+o["id"].(string)+"/confirm", nil, key...)
	require.Equal(fx.t, 200, code, a)
	return a
}

func (fx *fixture) process(o map[string]any, id string) map[string]any {
	fx.t.Helper()
	return fx.get("/api/sales/orders/" + o["id"].(string) + "/confirmations/" + id)
}

func branchOf(p map[string]any, component string) string {
	for _, b := range p["participants"].([]any) {
		if at(b, "component") == component {
			return at(b, "branch_id").(string)
		}
	}
	return ""
}

func (fx *fixture) orderState(id string) string {
	_, o := fx.alice.do("GET", "/api/sales/orders/"+id, nil)
	s, _ := o["state"].(string)
	return s
}

func (fx *fixture) get(path string) map[string]any {
	fx.t.Helper()
	code, out := fx.alice.do("GET", path, nil)
	require.Equal(fx.t, 200, code, out)
	return out
}

// publishOrgUnit publishes mdm.org_unit.changed.v1 as mdm/org's forwarder would.
func (fx *fixture) publishOrgUnit(unit map[string]any) {
	data, _ := json.Marshal(unit)
	id := unit["id"].(string)
	row := events.Row{EventID: ids.New().String(), Stream: orgStream.Name, Partition: orgStream.PartitionOf(id),
		Type: OrgUnitChanged, OccurredAt: time.Now(), RecordID: id, Version: unit["version"].(int64),
		Projection: events.Full, Payload: data, Seq: time.Now().UnixNano(), Epoch: events.Epoch{Generation: 1, Timeline: 1}}
	row.Digest = events.Digest(row.EventID, data)
	require.NoError(fx.t, fx.tr.Publish(fx.ctx, events.Message{Subject: events.Subject(orgStream.Name, row.Partition, row.Type),
		Headers: row.Headers(Org, orgStamp.GetInstance()), Data: data}))
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// portFree fails at once when a port the member needs is taken: a running cell or another member would answer in
// its place, and the test would assert against another process.
func portFree(t *testing.T, addrs ...string) {
	for _, a := range addrs {
		if !strings.Contains(a, ":") {
			a = "localhost:" + a
		}
		ln, err := net.Listen("tcp", a)
		require.NoError(t, err, fmt.Sprintf("port %s is taken: is a cell or another member running?", a))
		_ = ln.Close()
	}
}
