package service

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
)

// TestListMovements_非法游标映射成InvalidArgument：游标是调用方传回来的不透明
// 字符串，传坏了是入参错误（HTTP 400 / gRPC InvalidArgument），不是服务端故障。
func TestListMovements_非法游标映射成InvalidArgument(t *testing.T) {
	svc, r, db := newTestService(t)
	east := warehouseID(t, db, "WH-EAST")
	ctx := authedCtx(t, r, "svc-list-bad-cursor", east)

	for _, cursor := range []string{"!!!不是base64", "bm90LWEtY3Vyc29y"} { // 第二个是合法 base64、内容不是游标
		_, err := svc.ListMovements(ctx, repo.ListInput{Cursor: cursor, PageSize: 5})
		if err == nil {
			t.Fatalf("cursor=%q 应该被拒绝", cursor)
		}
		if got := status.Code(ToStatus(err)); got != codes.InvalidArgument {
			t.Fatalf("cursor=%q：非法游标应映射成 InvalidArgument，实际 %v（%v）", cursor, got, err)
		}
	}
}
