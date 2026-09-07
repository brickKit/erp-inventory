package repo

import "errors"

// pgError 是 pgx 错误类型的最小接口（同 be-sdk-go events.go 的
// isUniqueViolation 判据）：带 SQLSTATE 的错误都能用 errors.As 接住，
// 不需要直接依赖 pgconn 包。
type pgError interface{ SQLState() string }

// isForeignKeyViolation：SQLSTATE 23503，外键约束冲突——用来把"传了个
// 不存在的仓库"翻成 ErrInvalidArgument 而不是 Internal。
func isForeignKeyViolation(err error) bool {
	var pgErr pgError
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23503"
	}
	return false
}

// isUniqueViolation：SQLSTATE 23505，唯一约束冲突。
func isUniqueViolation(err error) bool {
	var pgErr pgError
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
