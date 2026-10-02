package tcc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
)

// FailureLevel 是一次命令失败该记的日志级别。ERROR 只留给运维必须处理的
// 事（依赖拨不通、上游内部错误、数据库出错）：
//   - 调用方自己的错误、会以 4xx 回给调用方的（参数不合法、订单不存在或
//     看不到、状态不对、上游明确拒绝如库存不足），以及调用方断开：INFO；
//   - 暂时性的（预留状态未定、上游超时或不可用），调用方带同一个幂等键
//     重试即可：WARN。
//
// 上游返回的 gRPC status 包在本组件的错误里也能认出来（status.FromError
// 会顺着 %w 往里找）。
func FailureLevel(err error) slog.Level {
	switch {
	case errors.Is(err, context.Canceled):
		return slog.LevelInfo
	case errors.Is(err, errReservePending), errors.Is(err, context.DeadlineExceeded):
		return slog.LevelWarn
	case errors.Is(err, repo.ErrInvalidArgument), errors.Is(err, repo.ErrNotFound),
		errors.Is(err, repo.ErrOrderNotDraft), errors.Is(err, repo.ErrOrderTerminal),
		errors.Is(err, repo.ErrForbidden):
		return slog.LevelInfo
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition, codes.OutOfRange,
			codes.AlreadyExists, codes.Aborted, codes.PermissionDenied, codes.Unauthenticated, codes.Canceled:
			return slog.LevelInfo
		case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
			return slog.LevelWarn
		}
	}
	return slog.LevelError
}

// automationFailureLevel 是赢单自动转单失败时的级别：业务上的拒绝会开一条
// 异常待办交给商机负责人，记 WARN；运维要处理的仍是 ERROR。
func automationFailureLevel(err error) slog.Level {
	if lv := FailureLevel(err); lv >= slog.LevelError {
		return lv
	}
	return slog.LevelWarn
}
