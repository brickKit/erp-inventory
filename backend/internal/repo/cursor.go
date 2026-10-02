package repo

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cursor 编码 (created_at, id)：keyset 分页，不是 offset——深分页不变慢、不跳行。
type cursorKey struct {
	CreatedAt time.Time
	ID        int64
}

// encodeCursor/decodeCursor 实现 keyset 分页的游标——只编码"最后一行的
// 排序键"，不透出任何 SQL 细节（同 mdm-product 的判据）。
func encodeCursor(k cursorKey) string {
	raw := k.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(k.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (cursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursorKey{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return cursorKey{}, fmt.Errorf("格式不对：%q", string(raw))
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return cursorKey{}, err
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return cursorKey{}, err
	}
	return cursorKey{CreatedAt: t, ID: id}, nil
}

// encodeIDCursor / decodeIDCursor 是按单个自增 id 排序的列表（余额列表）的游标。
func encodeIDCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("id|" + strconv.FormatInt(id, 10)))
}

func decodeIDCursor(s string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	rest, ok := strings.CutPrefix(string(raw), "id|")
	if !ok {
		return 0, fmt.Errorf("格式不对：%q", string(raw))
	}
	return strconv.ParseInt(rest, 10, 64)
}
