package module

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/be-sdk-go/migrate"
	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/brickKit/erp-sales/v2/migrations"
)

// 库身份（schema 与角色）只来自配置：部署方给一个 schema、一个对它有
// USAGE+CREATE 的登录角色，本组件就要能迁移进去、在里面读写——不管这两个名字
// 叫什么，也不管角色名是不是 "<schema>_rw"。
//
// 这里在测试库里现建一个非默认 schema 和一个名字跟它毫无关系的登录角色，用
// 这个角色跑本组件的迁移，再经 module.New 交回的 gRPC 面建一张单、读回来。
// 两种形态各走一遍：
//   - 独立运行：连接池以 PG_USER 本身登录；
//   - 进外壳：连接池以外壳自己的角色登录，这个角色只是 PG_USER 的成员且
//     NOINHERIT——不切到 PG_USER 就什么都读不到，所以能读写就证明每个事务
//     切的正是配置里的 PG_USER。
//
// 依赖的 mdm/customer、mdm/product 用测试进程里的假 gRPC 服务顶上：这里测的
// 是库身份，不是跨组件调用（那些由 make test-cross 对真依赖测）。
func TestNew_库身份只来自配置_非默认schema与任意名字的登录角色(t *testing.T) {
	adminDSN := os.Getenv("TEST_PG_DSN")
	if adminDSN == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	ctx := context.Background()
	id := newDBIdentity(t, adminDSN)

	env := id.pgEnv(id.role)
	for i := 0; i < 2; i++ { // 连跑两次：迁移必须幂等
		if err := migrate.Run(ctx, env, []string{"up"}, migrations.FS); err != nil {
			t.Fatalf("以登录角色 %s 迁移进 schema %s 失败（第 %d 次）：%v", id.role, id.schema, i+1, err)
		}
	}

	// 迁移以这个角色运行，表就属于它；运行中的组件要给分区父表建新分区，
	// 靠的正是这一点，不需要迁移里写任何 OWNER TO。
	var foreign []string
	rows, err := id.admin.QueryContext(ctx, `
		SELECT c.relname || ' → ' || pg_get_userbyid(c.relowner)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
		  AND pg_get_userbyid(c.relowner) <> $2
		ORDER BY 1`, id.schema, id.role)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		foreign = append(foreign, s)
	}
	rows.Close()
	if len(foreign) > 0 {
		t.Fatalf("schema %s 里有不属于迁移角色 %s 的对象：%v", id.schema, id.role, foreign)
	}

	t.Run("独立运行_连接池以PG_USER登录", func(t *testing.T) {
		createAndRead(t, id, id.open(t, id.role))
	})
	t.Run("进外壳_连接池以外壳角色登录_事务里切到PG_USER", func(t *testing.T) {
		createAndRead(t, id, id.open(t, id.shellRole))
	})
}

// createAndRead 用 module.New 装出本组件，经它注册的 gRPC 面写一张单、读回来，
// 再用管理连接确认这张单落在配置的 schema 里。
func createAndRead(t *testing.T, id *dbIdentity, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	deps := startFakeMDM(t)

	rt := besdk.NewShellRuntime(besdk.ShellModuleConfig{
		ComponentID: "erp/sales", ComponentVersion: "2.0.0",
		Env: map[string]string{
			"PG_SCHEMA": id.schema, "PG_USER": id.role,
			"DEFAULT_WAREHOUSE_ID":       "1",
			"MDM_CUSTOMER_GRPC_ENDPOINT": "http://" + deps,
			"MDM_PRODUCT_GRPC_ENDPOINT":  "http://" + deps,
		},
	}, db, nil)
	mod, err := New(ctx, rt)
	if err != nil {
		t.Fatalf("module.New：%v", err)
	}

	// 生产里调用者身份由 SDK 的拦截器从 JWT 里取；这里直接塞进 ctx。
	claims := besdk.Claims{Sub: "u_r63", DeptPath: "/r63/"}
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(besdk.ContextWithClaims(ctx, claims), req)
	}))
	mod.RegisterGRPC(srv)
	addr := serve(t, srv)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	sales := salesv1.NewSalesServiceClient(conn)

	created, err := sales.CreateOrder(ctx, &salesv1.CreateOrderRequest{
		IdempotencyKey: fmt.Sprintf("r63-%d", time.Now().UnixNano()),
		CustomerId:     "C-r63",
		Items:          []*salesv1.CreateOrderItemInput{{ProductId: "P-r63", Qty: "2"}},
	})
	if err != nil {
		t.Fatalf("建单（写）失败：%v", err)
	}
	got, err := sales.GetOrder(ctx, &salesv1.GetOrderRequest{Id: created.Order.Id})
	if err != nil {
		t.Fatalf("读回订单失败：%v", err)
	}
	if got.Id != created.Order.Id || got.CustomerId != "C-r63" || got.Status != salesv1.OrderStatus_ORDER_STATUS_DRAFT {
		t.Fatalf("读回的订单不对：%+v", got)
	}

	var n int
	q := fmt.Sprintf(`SELECT count(*) FROM %s.sales_orders WHERE id = $1`, id.schema)
	if err := id.admin.QueryRowContext(ctx, q, created.Order.Id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("订单应落在配置的 schema %s 里，实际找到 %d 行", id.schema, n)
	}
}

// dbIdentity 是测试现建的一套库身份：一个 schema、一个对它有 USAGE+CREATE 的
// 登录角色（迁移与业务都用它），外加一个只是它的成员、NOINHERIT 的"外壳"登录角色。
type dbIdentity struct {
	admin                   *sql.DB
	adminURL                *url.URL
	schema, role, shellRole string
	password                string
}

func newDBIdentity(t *testing.T, adminDSN string) *dbIdentity {
	t.Helper()
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	sfx := hex.EncodeToString(buf)
	pwBuf := make([]byte, 16)
	if _, err := rand.Read(pwBuf); err != nil {
		t.Fatal(err)
	}
	id := &dbIdentity{
		admin: admin, adminURL: u, password: hex.EncodeToString(pwBuf),
		schema: "r63_sales_" + sfx,
		// 故意不叫 "<schema>_rw"：角色名只能来自 PG_USER。
		role:      "r63_owner_" + sfx,
		shellRole: "r63_shell_" + sfx,
	}

	// 先登记清理，再建：中途失败也要把已建的东西删掉。连接池的 Cleanup 后登记、
	// 先执行，所以删角色时这些角色已经没有连接了。
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.ExecContext(c, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename IN ($1, $2)`, id.role, id.shellRole)
		for _, stmt := range []string{
			"DROP SCHEMA IF EXISTS " + id.schema + " CASCADE",
			"DROP ROLE IF EXISTS " + id.shellRole,
			"DROP OWNED BY " + id.role,
			"DROP ROLE IF EXISTS " + id.role,
		} {
			if _, err := admin.ExecContext(c, stmt); err != nil && !strings.Contains(err.Error(), "does not exist") {
				t.Errorf("清理 %q 失败：%v", stmt, err)
			}
		}
		admin.Close()
	})
	for _, stmt := range []string{
		"CREATE SCHEMA " + id.schema,
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", id.role, id.password),
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA %s TO %s", id.schema, id.role),
		fmt.Sprintf("CREATE ROLE %s LOGIN NOINHERIT PASSWORD '%s'", id.shellRole, id.password),
		fmt.Sprintf("GRANT %s TO %s", id.role, id.shellRole),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("准备库身份失败（%s）：%v", strings.SplitN(stmt, " PASSWORD", 2)[0], err)
		}
	}
	return id
}

// pgEnv 是迁移入口要的 PG_* 键（以 user 登录）。
func (id *dbIdentity) pgEnv(user string) map[string]string {
	return map[string]string{
		"PG_HOST": id.adminURL.Hostname(), "PG_PORT": id.adminURL.Port(),
		"PG_DATABASE": strings.TrimPrefix(id.adminURL.Path, "/"),
		"PG_USER":     user, "PG_PASSWORD": id.password, "PG_SCHEMA": id.schema,
	}
}

// open 以 user 登录开一个连接池。
func (id *dbIdentity) open(t *testing.T, user string) *sql.DB {
	t.Helper()
	dsn, err := besdk.PGDSN(besdk.NewConfig(id.pgEnv(user)))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// startFakeMDM 起一个同时扮演 mdm/customer 与 mdm/product 的 gRPC 服务：
// 任何 id 都是一个 ACTIVE 的客户 / 产品。返回 host:port。
func startFakeMDM(t *testing.T) string {
	t.Helper()
	srv := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(srv, fakeCustomers{})
	productv1.RegisterProductServiceServer(srv, fakeProducts{})
	return serve(t, srv)
}

func serve(t *testing.T, srv *grpc.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

type fakeCustomers struct {
	customerv1.UnimplementedCustomerServiceServer
}

func (fakeCustomers) BatchGet(_ context.Context, req *customerv1.BatchGetRequest) (*customerv1.BatchGetResponse, error) {
	out := &customerv1.BatchGetResponse{}
	for _, id := range req.Ids {
		out.Customers = append(out.Customers, &customerv1.Customer{
			Id: id, Name: "库身份测试客户", CreditLimit: "100000.00", Version: 1,
			Status: customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type fakeProducts struct {
	productv1.UnimplementedProductServiceServer
}

func (fakeProducts) BatchGet(_ context.Context, req *productv1.BatchGetRequest) (*productv1.BatchGetResponse, error) {
	out := &productv1.BatchGetResponse{}
	for _, id := range req.Ids {
		out.Products = append(out.Products, &productv1.Product{
			Id: id, Sku: "SKU-" + id, Name: "库身份测试产品", BaseUomId: "EA", Version: 1,
			Status: productv1.ProductStatus_PRODUCT_STATUS_ACTIVE,
		})
	}
	return out, nil
}

// 新库迁移完、还没等后台分区维护跑第一轮，就要能写入：迁移里写死的初始分区
// 只覆盖写迁移那几周（2026-09 起），哪天在一个新库上装本组件，当周 / 当月的
// 分区就不在了，维护任务第一轮跑完之前的写入会报 no partition of relation。
// 所以迁移跑完那一刻，四张分区表都要有当前周期和下一个周期的分区（再往后由
// 维护任务提前建）。
func TestMigrate_新schema迁移完就有当前与下一个周期的分区(t *testing.T) {
	adminDSN := os.Getenv("TEST_PG_DSN")
	if adminDSN == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	ctx := context.Background()
	id := newDBIdentity(t, adminDSN)
	if err := migrate.Run(ctx, id.pgEnv(id.role), []string{"up"}, migrations.FS); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}

	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	wd := int(now.Weekday())
	if wd == 0 {
		wd = 7
	}
	week := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(wd - 1))

	var want []string
	for _, from := range []time.Time{month, month.AddDate(0, 1, 0)} {
		for _, table := range []string{"sales_orders", "sales_order_items"} {
			want = append(want, table+"_"+from.Format("2006_01_02"))
		}
	}
	for _, from := range []time.Time{week, week.AddDate(0, 0, 7)} {
		for _, table := range []string{"event_outbox", "event_inbox"} {
			want = append(want, table+"_"+from.Format("2006_01_02"))
		}
	}
	var missing []string
	for _, name := range want {
		var exists bool
		if err := id.admin.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, id.schema+"."+name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("迁移完缺这些分区（今天 %s）：%v", now.Format("2006-01-02"), missing)
	}
}
