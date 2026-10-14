package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------------------------------------------------------------------------
// 假 MySQL：slow=true 时每条语句都卡住（模拟 2026-10-14 friend.vercode 全表扫 45~64s），
// 直到 context 结束或 release 被关闭；slow=false 时立即返回一行。
// context 打断时与 go-sql-driver 一致：连接作废（IsValid=false），由 database/sql 丢弃重建。
// ---------------------------------------------------------------------------

type fakeDB struct {
	slow    atomic.Bool
	release chan struct{}
	opened  atomic.Int64
}

func newFakeDB() *fakeDB { return &fakeDB{release: make(chan struct{})} }

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) {
	f.opened.Add(1)
	return &fakeConn{db: f}, nil
}
func (f *fakeDB) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type fakeConn struct {
	db  *fakeDB
	bad atomic.Bool
}

func (c *fakeConn) wait(ctx context.Context) error {
	if !c.db.slow.Load() {
		return nil
	}
	select {
	case <-ctx.Done():
		c.bad.Store(true)
		return ctx.Err()
	case <-c.db.release:
		return nil
	}
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return c, nil }
func (c *fakeConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	return c, c.wait(ctx)
}
func (c *fakeConn) Commit() error   { return nil }
func (c *fakeConn) Rollback() error { return nil }
func (c *fakeConn) IsValid() bool   { return !c.bad.Load() }
func (c *fakeConn) ResetSession(context.Context) error {
	if c.bad.Load() {
		return driver.ErrBadConn
	}
	return nil
}
func (c *fakeConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return &fakeRows{}, nil
}
func (c *fakeConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"v"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

func testOptions(maxOpen int, timeout time.Duration) MySQLOptions {
	return MySQLOptions{MaxOpenConns: maxOpen, MaxIdleConns: maxOpen, QueryTimeout: timeout}
}

// runConcurrently 并发跑 n 次 fn，返回每次的错误与最长耗时
func runConcurrently(n int, fn func() error) ([]error, time.Duration) {
	var wg sync.WaitGroup
	errs := make([]error, n)
	durs := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			errs[i] = fn()
			durs[i] = time.Since(start)
		}(i)
	}
	wg.Wait()
	var max time.Duration
	for _, d := range durs {
		if d > max {
			max = d
		}
	}
	return errs, max
}

// 复现事故前的行为：没有超时时，池被慢查询占满后后来的请求无限期排队。
func TestPoolWithoutTimeoutHangs(t *testing.T) {
	f := newFakeDB()
	sqlDB := sql.OpenDB(f)
	defer sqlDB.Close()
	sess := NewSessionWithOptions(sqlDB, testOptions(2, 0))
	f.slow.Store(true)

	done := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			var v int
			_, err := sess.Select("v").From("t").Load(&v)
			done <- err
		}()
	}
	select {
	case err := <-done:
		t.Fatalf("没有超时时请求不应返回，实际返回 %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if s := sqlDB.Stats(); s.InUse != 2 || s.WaitCount == 0 {
		t.Fatalf("应当 2 条连接被占满且有排队，实际 %+v", s)
	}
	close(f.release) // 清理：放行卡住的查询
	for i := 0; i < 3; i++ {
		<-done
	}
}

// 池耗尽 + 慢查询：每个请求（含排队等连接的）都在 QueryTimeout 内失败；
// 慢查询消失后不重启，池自己恢复，坏连接被丢弃重建，没有泄漏。
func TestPoolExhaustionFailsFastAndRecovers(t *testing.T) {
	const timeout = 200 * time.Millisecond
	f := newFakeDB()
	sqlDB := sql.OpenDB(f)
	defer sqlDB.Close()
	sess := NewSessionWithOptions(sqlDB, testOptions(2, timeout))

	f.slow.Store(true)
	load := func() error {
		var v int
		_, err := sess.Select("v").From("t").Load(&v)
		return err
	}
	errs, maxDur := runConcurrently(20, load) // 20 个请求抢 2 条连接
	for i, err := range errs {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("请求 %d 应以超时失败，实际 %v", i, err)
		}
	}
	if maxDur > timeout+300*time.Millisecond {
		t.Fatalf("最慢请求 %v，超过超时上限 %v 太多", maxDur, timeout)
	}

	// 有界事务在池满时同样按时失败，而不是 Begin() 无限排队
	hold := make(chan struct{})
	var holders sync.WaitGroup
	for i := 0; i < 2; i++ {
		holders.Add(1)
		go func() { defer holders.Done(); close(hold); _ = load() }()
		<-hold
		hold = make(chan struct{})
	}
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	_, cancel, err := BeginTxTimeout(sess, 0)
	cancel()
	if err == nil || time.Since(start) > timeout+300*time.Millisecond {
		t.Fatalf("池满时 BeginTxTimeout 应在 %v 内失败，实际 err=%v 耗时 %v", timeout, err, time.Since(start))
	}
	holders.Wait()

	// 数据库恢复：不重启，请求全部成功
	f.slow.Store(false)
	errs, _ = runConcurrently(50, load)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("恢复后请求 %d 失败：%v", i, err)
		}
	}
	tx, cancel, err := BeginTxTimeout(sess, 0)
	if err != nil {
		t.Fatalf("恢复后开事务失败：%v", err)
	}
	if _, err := tx.InsertInto("t").Pair("v", 1).Exec(); err != nil {
		t.Fatalf("恢复后事务内写失败：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("恢复后提交失败：%v", err)
	}
	cancel()

	s := sqlDB.Stats()
	if s.InUse != 0 || s.OpenConnections > 2 {
		t.Fatalf("恢复后不应有占用或超额连接，实际 %+v", s)
	}
	if f.opened.Load() <= 2 {
		t.Fatalf("超时打断的连接应被丢弃重建，实际只建过 %d 条", f.opened.Load())
	}
}

// 嵌套取连接：持有事务时再向同一个池要连接。池只有 1 条时这是必然死等；
// 有超时后变成按时失败，事务回滚后池恢复。
func TestNestedAcquireIsBounded(t *testing.T) {
	const timeout = 200 * time.Millisecond
	f := newFakeDB()
	sqlDB := sql.OpenDB(f)
	defer sqlDB.Close()
	sess := NewSessionWithOptions(sqlDB, testOptions(1, timeout))

	tx, cancel, err := BeginTxTimeout(sess, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var v int
	_, err = sess.Select("v").From("t").Load(&v) // 事务外走 session：第二条连接
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > timeout+300*time.Millisecond {
		t.Fatalf("嵌套取连接应在 %v 内超时，实际 err=%v 耗时 %v", timeout, err, time.Since(start))
	}
	_ = tx.Rollback()
	cancel()
	if _, err := sess.Select("v").From("t").Load(&v); err != nil {
		t.Fatalf("事务结束后池应恢复：%v", err)
	}
	if s := sqlDB.Stats(); s.InUse != 0 {
		t.Fatalf("不应残留占用连接：%+v", s)
	}
}

func newFakeGorm(t *testing.T, f *fakeDB, maxOpen int, timeout time.Duration) (*gorm.DB, *sql.DB) {
	sqlDB := sql.OpenDB(f)
	ConfigurePool(sqlDB, testOptions(maxOpen, timeout))
	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}),
		&gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterGormStatementTimeout(gdb, timeout); err != nil {
		t.Fatal(err)
	}
	return gdb, sqlDB
}

// ManagerDB（gorm）同样：池满 + 慢查询时 Pluck（IsCompanyWebShowAsApp 的写法）按时失败，恢复后自愈。
func TestGormStatementTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	f := newFakeDB()
	gdb, sqlDB := newFakeGorm(t, f, 2, timeout)
	defer sqlDB.Close()

	f.slow.Store(true)
	pluck := func() error {
		var v int8
		return gdb.Table("company_config").Where("company_code=?", "x").Pluck("web_show_as_app", &v).Error
	}
	errs, maxDur := runConcurrently(20, pluck)
	for i, err := range errs {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("请求 %d 应以超时失败，实际 %v", i, err)
		}
	}
	if maxDur > timeout+300*time.Millisecond {
		t.Fatalf("最慢请求 %v，超过超时上限 %v 太多", maxDur, timeout)
	}
	// 写操作走 gorm 默认事务：Begin 也在超时内
	start := time.Now()
	err := gdb.Table("company_device").Where("device_code=?", "d").Update("default", 0).Error
	if err == nil || time.Since(start) > timeout+300*time.Millisecond {
		t.Fatalf("慢库上的更新应在 %v 内失败，实际 err=%v 耗时 %v", timeout, err, time.Since(start))
	}

	f.slow.Store(false)
	errs, _ = runConcurrently(50, pluck)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("恢复后请求 %d 失败：%v", i, err)
		}
	}
	// 链式复用同一个 *gorm.DB：上一次执行的超时 context 必须被还原，否则第二次拿到已取消的 context
	q := gdb.Table("t").Where("a=?", 1)
	var n int64
	if err := q.Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	var rows []map[string]interface{}
	if err := q.Find(&rows).Error; err != nil {
		t.Fatalf("链式复用第二次执行失败（超时 context 未还原？）：%v", err)
	}
	// 调用方自带 deadline 的不覆盖
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gdb.WithContext(ctx).Table("t").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if s := sqlDB.Stats(); s.InUse != 0 {
		t.Fatalf("不应残留占用连接：%+v", s)
	}
}

func TestApplyMySQLDSNTimeouts(t *testing.T) {
	o := DefaultMySQLOptions(10, 5, time.Hour)
	dsn, err := ApplyMySQLDSNTimeouts("im:p@ss@tcp(rds:3306)/zeroim?parseTime=True&loc=Local", o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"parseTime=true", "loc=Local", "timeout=3s", "readTimeout=15s", "writeTimeout=15s",
		"max_execution_time=8000", "innodb_lock_wait_timeout=10"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN 缺少 %q：%s", want, dsn)
		}
	}
	// DSN 里显式写了的参数不覆盖
	dsn, err = ApplyMySQLDSNTimeouts("u:p@tcp(h:3306)/d?readTimeout=30s&max_execution_time=2000", o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "readTimeout=30s") || !strings.Contains(dsn, "max_execution_time=2000") ||
		strings.Contains(dsn, "max_execution_time=8000") {
		t.Fatalf("显式参数被覆盖：%s", dsn)
	}
	// 时长 <=0 关闭对应层
	off := o
	off.ReadTimeout, off.MaxExecutionTime = -1, 0
	dsn, err = ApplyMySQLDSNTimeouts("u:p@tcp(h:3306)/d", off)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dsn, "readTimeout") || strings.Contains(dsn, "max_execution_time") {
		t.Fatalf("关闭的超时不应写入：%s", dsn)
	}
}
