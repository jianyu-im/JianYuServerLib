package db

import (
	"context"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
	"gorm.io/gorm"
)

// BeginTxTimeout 开启有界的 dbr 事务：等连接 + 事务内全部语句 + Commit 都必须在 timeout 内完成，
// 超时 database/sql 自动回滚并归还连接。timeout<=0 时沿用会话的 QueryTimeout。
// 返回的 cancel 必须在事务结束后调用（defer cancel()）。
//
// dbr 的 Session.Begin() 用的是 context.Background()，池满时会无限期排队；请求路径上的事务都应走这里。
func BeginTxTimeout(sess *dbr.Session, timeout time.Duration) (*dbr.Tx, context.CancelFunc, error) {
	if timeout <= 0 {
		timeout = sess.GetTimeout()
	}
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	tx, err := sess.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return tx, cancel, nil
}

const (
	gormTimeoutCancelKey = "zeroim:stmt_timeout_cancel"
	gormTimeoutOrigKey   = "zeroim:stmt_timeout_orig_ctx"
)

// RegisterGormStatementTimeout 给 gorm 的 Create/Query/Update/Delete/Raw 每次执行套上
// context 超时（覆盖等连接 + 执行；写操作含 gorm 默认事务的 Begin/Commit）。
// 调用方已经带了 deadline 的 context 不受影响；Row/Rows 不套（结果集要在返回后继续读）。
//
// 执行结束后把 Statement.Context 还原，链式复用同一个 *gorm.DB（先 Count 再 Find）不会拿到已取消的 context。
func RegisterGormStatementTimeout(gdb *gorm.DB, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	before := func(tx *gorm.DB) {
		if tx.Statement == nil {
			return
		}
		orig := tx.Statement.Context
		if orig == nil {
			orig = context.Background()
		}
		if _, ok := orig.Deadline(); ok {
			return
		}
		ctx, cancel := context.WithTimeout(orig, timeout)
		tx.Statement.Context = ctx
		tx.InstanceSet(gormTimeoutCancelKey, cancel)
		tx.InstanceSet(gormTimeoutOrigKey, orig)
	}
	after := func(tx *gorm.DB) {
		if tx.Statement == nil {
			return
		}
		if v, ok := tx.InstanceGet(gormTimeoutCancelKey); ok {
			if cancel, ok := v.(context.CancelFunc); ok {
				cancel()
			}
		}
		if v, ok := tx.InstanceGet(gormTimeoutOrigKey); ok {
			if orig, ok := v.(context.Context); ok {
				tx.Statement.Context = orig
			}
		}
		// InstanceSet 的键是 "%p(Statement)"+key；用完即删，避免同一 Statement 下次执行误读到旧值
		prefix := fmt.Sprintf("%p", tx.Statement)
		tx.Statement.Settings.Delete(prefix + gormTimeoutCancelKey)
		tx.Statement.Settings.Delete(prefix + gormTimeoutOrigKey)
	}
	cb := gdb.Callback()
	type reg interface {
		Register(name string, fn func(*gorm.DB)) error
	}
	for _, p := range []struct{ b, a reg }{
		{cb.Create().Before("*"), cb.Create().After("*")},
		{cb.Query().Before("*"), cb.Query().After("*")},
		{cb.Update().Before("*"), cb.Update().After("*")},
		{cb.Delete().Before("*"), cb.Delete().After("*")},
		{cb.Raw().Before("*"), cb.Raw().After("*")},
	} {
		if err := p.b.Register("zeroim:stmt_timeout_before", before); err != nil {
			return err
		}
		if err := p.a.Register("zeroim:stmt_timeout_after", after); err != nil {
			return err
		}
	}
	return nil
}
