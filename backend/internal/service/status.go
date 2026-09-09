package service

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/tcc"
)

// ToStatus 把 repo/tcc 层的哨兵错误翻成 gRPC status——HTTP 与 gRPC 两条
// 对外接口共用同一套业务错误类型（同 erp-finance/erp-inventory 的判据）。
//
// ⚠️ 本组件比其余组件多一层特殊情况：err 可能是"透传"自四条依赖边某一
// 次远程调用本身返回的 gRPC status（如 erp-inventory 的
// FailedPrecondition：库存不足）。这类错误不该被下面 default 分支压成
// codes.Internal——那样会把"库存不足"这种业务性、调用方本该能识别的
// 失败原因，变成一个看起来像系统内部错误的 500。所以在真正走到
// default 之前，先检查 err 是不是已经带着一个真实的 gRPC status，是的
// 话原样透传。
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, repo.ErrOrderNotDraft), errors.Is(err, repo.ErrOrderTerminal):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, tcc.ErrReservePending):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, repo.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, repo.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	}
	// 透传下游依赖组件（mdm-customer/mdm-product/erp-inventory/erp-finance）
	// 已经返回的真实 gRPC status——ok==false 说明 err 根本不是一个 gRPC
	// status（比如本地校验错误、context.DeadlineExceeded 之类的裸 error），
	// 这种才落到最后的 codes.Internal。
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}
