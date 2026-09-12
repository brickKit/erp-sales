package repo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// modelStep 是订单状态机的独立复述（不是照抄 write.go 的实现），用来跟
// 真实落库结果交叉核对——这是 04-testing-standard.md §3.2 说的"删掉重写
// 也该成立"的判据：这里描述的是订单状态机*应该*怎么走，不是"这段 Go
// 代码现在怎么写的"。
//
//	DRAFT     -confirm-> CONFIRMED（唯一合法出边）
//	DRAFT     -cancel->  CANCELLED
//	CONFIRMED -cancel->  CANCELLED
//	CONFIRMED -ship->    SHIPPED
//	CANCELLED -cancel->  CANCELLED（幂等空操作，不是错误）
//	其余任意 (状态, 操作) 组合一律拒绝，状态原地不动
func modelStep(cur, op string) (next string, ok bool) {
	switch op {
	case "confirm":
		if cur == StatusDraft {
			return StatusConfirmed, true
		}
		return cur, false
	case "cancel":
		switch cur {
		case StatusDraft, StatusConfirmed:
			return StatusCancelled, true
		case StatusCancelled:
			return StatusCancelled, true
		default:
			return cur, false
		}
	case "ship":
		if cur == StatusConfirmed {
			return StatusShipped, true
		}
		return cur, false
	default:
		return cur, false
	}
}

// TestProperty_订单状态机不允许法外转移 是 04-testing-standard.md §3.2 对
// "核心交易类组件"的强制要求。此前这条状态机只有一堆具体例子在测
// （TestFinalizeConfirm_DRAFT到CONFIRMED且发事件、
// TestFinalizeCancel_已终态不能取消……），每条例子都是"我想到的这一种
// 情形"，从没有随机操作序列攻击过它——比如"confirm 两次"“cancel 之后再
// ship"这类组合，例子测试很容易漏掉某一种没写到。
//
// 用 rapid 生成随机的 confirm/cancel/ship 操作序列，每一步都跟 modelStep
// 这个独立模型交叉核对：真实调用成功与否是否与模型预测一致、调用后的
// 真实 status 是否与模型预测的下一状态一致。FinalizeConfirm 传的
// reservation_id 是随便造的字符串——这一层本来就不校验它（TCC 编排在
// 调用这里之前已经把 Reserve 做完，见 write.go 顶部注释），不影响状态机
// 本身的正确性。
func TestProperty_订单状态机不允许法外转移(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")

	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		orderIDPrefix := uniqueSuffix("prop-fsm")

		order, err := r.CreateOrder(ctx, CreateOrderInput{
			IdempotencyKey: "create-" + orderIDPrefix, CustomerID: "C-prop", CustomerName: "属性测试客户",
			Items: testItems(),
		})
		if err != nil {
			rt.Fatalf("建单失败：%v", err)
		}

		cur := StatusDraft
		n := rapid.IntRange(3, 15).Draw(rt, "opCount")
		for i := 0; i < n; i++ {
			op := rapid.SampledFrom([]string{"confirm", "cancel", "ship"}).Draw(rt, "op")
			wantNext, wantOK := modelStep(cur, op)

			var gotErr error
			key := fmt.Sprintf("%s-%s-%d", op, orderIDPrefix, i)
			switch op {
			case "confirm":
				_, gotErr = r.FinalizeConfirm(ctx, FinalizeConfirmInput{
					IdempotencyKey: key, OrderID: order.ID, ReservationID: "prop-fake-reservation-" + key,
				})
			case "cancel":
				_, gotErr = r.FinalizeCancel(ctx, FinalizeCancelInput{
					IdempotencyKey: key, OrderID: order.ID, Reason: "property test",
				})
			case "ship":
				_, gotErr = r.FinalizeShip(ctx, FinalizeShipInput{
					IdempotencyKey: key, OrderID: order.ID,
				})
			}

			if wantOK && gotErr != nil {
				rt.Fatalf("第 %d 步：模型认为 %s 从 %s 应该成功，真实调用却报错：%v", i, op, cur, gotErr)
			}
			if !wantOK && gotErr == nil {
				rt.Fatalf("第 %d 步：模型认为 %s 从 %s 应该被拒绝，真实调用却成功了——这是状态机允许了一次法外转移", i, op, cur)
			}
			if !wantOK && !errors.Is(gotErr, ErrOrderNotDraft) && !errors.Is(gotErr, ErrOrderTerminal) {
				rt.Fatalf("第 %d 步：%s 从 %s 被拒绝，但错误既不是 ErrOrderNotDraft 也不是 ErrOrderTerminal：%v", i, op, cur, gotErr)
			}

			got, err := r.GetOrder(ctx, order.ID)
			if err != nil {
				rt.Fatalf("第 %d 步：GetOrder 失败：%v", i, err)
			}
			if got.Status != wantNext {
				rt.Fatalf("第 %d 步：%s 操作之后，模型预测状态是 %s，真实落库状态是 %s（操作前状态 %s）",
					i, op, wantNext, got.Status, cur)
			}
			cur = wantNext
		}
	})
}

// TestProperty_CreateOrder并发同key仅执行一次 补的是 04-testing-standard.md
// §3.2 明确要求、此前项目里从没写过的那一半幂等性测试："多个并发请求带着
// 同一个 idempotency_key 同时到达"，而不是"串行重放同一个 key 两次"
// （已有的 TestCreateOrder_幂等 只验证了串行重放）。CreateOrder 是四个写
// 命令里唯一不需要跨组件 gRPC 就能独立验证的一个，用它攻击
// claimIdempotency 的 `INSERT ... ON CONFLICT DO NOTHING` 判据。
func TestProperty_CreateOrder并发同key仅执行一次(t *testing.T) {
	db := testDB(t)
	db.SetMaxOpenConns(50)
	r := New(db, "erp_sales_rw", "erp_sales")

	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		concurrency := rapid.IntRange(2, 20).Draw(rt, "concurrency")
		key := uniqueSuffix("prop-create-idem")

		results := make([]*Order, concurrency)
		errs := make([]error, concurrency)
		var wg sync.WaitGroup
		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			i := i
			go func() {
				defer wg.Done()
				results[i], errs[i] = r.CreateOrder(ctx, CreateOrderInput{
					IdempotencyKey: key, CustomerID: "C-prop-idem", CustomerName: "属性测试并发客户",
					Items: testItems(),
				})
			}()
		}
		wg.Wait()

		var firstID string
		for i, err := range errs {
			if err != nil {
				rt.Fatalf("并发 CreateOrder 用同一个 idempotency_key，goroutine %d 不该报错：%v", i, err)
			}
			if firstID == "" {
				firstID = results[i].ID
			} else if results[i].ID != firstID {
				rt.Fatalf("幂等性被打破：同一个 idempotency_key 的 %d 次并发调用应返回同一个 order id，实际出现 %q 与 %q 两个不同的值",
					concurrency, firstID, results[i].ID)
			}
		}

		orderID, err := strconv.ParseInt(firstID, 10, 64)
		if err != nil {
			rt.Fatalf("order id 不是数字：%q", firstID)
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM erp_sales.sales_orders WHERE id = $1`, orderID).Scan(&n); err != nil {
			rt.Fatalf("查 sales_orders 失败：%v", err)
		}
		if n != 1 {
			rt.Fatalf("幂等性被打破：id=%s 期望恰好 1 行，实际 %d 行", firstID, n)
		}
	})
}
