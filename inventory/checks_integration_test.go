//go:build integration

package inventory

import (
	"context"
	"strconv"
	"testing"

	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/recoverypb"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// checksStory is a full recovery's checks against this member (be-protocol chapter 14): the recovery tool only; the
// audit of a business change names the object's version after it (P14.3), and the owner answers whether the object
// is still there at that version.
func checksStory(t *testing.T, w *sectest.World, admin *pgx.Conn, typ, id string) {
	ctx := context.Background()
	dial := func(as string) recoverypb.CheckServiceClient {
		conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(w.TLS(t, as).ClientConfig(ID))))
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })
		return recoverypb.NewCheckServiceClient(conn)
	}
	_, err := dial("mdm/partner").Check(ctx, &recoverypb.CheckRequest{Recovery: "r"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the recovery tool only: %v", err)
	cs := dial("ops/recover")
	checked, err := cs.Check(ctx, &recoverypb.CheckRequest{Recovery: "r"})
	require.NoError(t, err)
	assert.NotEmpty(t, checked.GetResults())

	var version string
	require.NoError(t, admin.QueryRow(ctx, "SELECT target->>'version' FROM "+itSchema+".be_audit_log WHERE target->>'type' = $1 "+
		"AND target->>'id' = $2 ORDER BY ord DESC LIMIT 1", typ, id).Scan(&version), "the audit names the version")
	v, err := strconv.ParseInt(version, 10, 64)
	require.NoError(t, err)
	present, err := cs.Present(ctx, &recoverypb.PresentRequest{Recovery: "r", Objects: []*recoverypb.ObjectRef{
		{Type: typ, Id: id, Version: version}, {Type: typ, Id: id, Version: strconv.FormatInt(v+1, 10)},
		{Type: typ, Id: ids.New().String(), Version: "1"}}})
	require.NoError(t, err)
	var answers []string
	for _, a := range present.GetAnswers() {
		answers = append(answers, a.GetAnswer())
	}
	assert.Equal(t, []string{"present", "older", "absent"}, answers)
}

// movementPresent: a movement has no version; it is present or absent.
func movementPresent(t *testing.T, w *sectest.World, admin *pgx.Conn) {
	ctx := context.Background()
	var id string
	require.NoError(t, admin.QueryRow(ctx, "SELECT id::text FROM "+itSchema+".movement LIMIT 1").Scan(&id))
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(w.TLS(t, "ops/recover").ClientConfig(ID))))
	require.NoError(t, err)
	defer conn.Close()
	got, err := recoverypb.NewCheckServiceClient(conn).Present(ctx, &recoverypb.PresentRequest{Recovery: "r",
		Objects: []*recoverypb.ObjectRef{{Type: Movements, Id: id}, {Type: Movements, Id: ids.New().String()}}})
	require.NoError(t, err)
	assert.Equal(t, "present", got.GetAnswers()[0].GetAnswer())
	assert.Equal(t, "absent", got.GetAnswers()[1].GetAnswer())
}
