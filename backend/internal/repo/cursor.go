package repo

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cursor 编码 (created_at, id)：keyset 分页，不是 offset（决策 53）。
type cursorKey struct {
	CreatedAt time.Time
	ID        int64
}

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
