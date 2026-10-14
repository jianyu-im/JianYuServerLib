package db

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	migrate "github.com/rubenv/sql-migrate"
)

// MySQLOptions 连接池与超时参数。
//
// 2026-10-14 事故（friend.vercode 全表扫 45~64s）的教训：池里每条连接都没有任何超时，
// 一条慢 SQL 能把连接占一分钟，几千个请求在 database/sql.(*DB).conn 里无限期排队，
// 数据库恢复后积压也不会自己消掉。这里的每一层都是有界的：
//   - QueryTimeout：dbr 每条语句的 context 超时，**包含排队等连接的时间**，池满时请求按时失败而不是死等；
//   - MaxExecutionTime：服务端只读 SELECT 执行上限（MySQL max_execution_time），超时由 RDS 自己杀掉，连接仍可复用；
//   - LockWaitTimeout：服务端行锁等待上限（innodb_lock_wait_timeout），写语句不再陪长事务等 50s；
//   - ReadTimeout/WriteTimeout：驱动层网络读写兜底（含写语句、网络黑洞），超时连接被丢弃；
//   - DialTimeout：建连上限。
//
// 任一时长 <=0 表示该层不启用。DSN 里已显式写了的同名参数优先，不会被覆盖。
type MySQLOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration

	QueryTimeout     time.Duration
	DialTimeout      time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxExecutionTime time.Duration
	LockWaitTimeout  time.Duration
}

// 默认超时：SELECT 在服务端 8s 被杀；客户端整条语句（含等连接）10s 放弃；网络读写 15s 兜底。
// 量级顺序 MaxExecutionTime < QueryTimeout < ReadTimeout，保证慢 SELECT 优先以服务端
// 3024 错误干净结束（连接可复用），而不是客户端掐断连接导致重连风暴。
const (
	DefaultMySQLQueryTimeout     = 10 * time.Second
	DefaultMySQLDialTimeout      = 3 * time.Second
	DefaultMySQLReadTimeout      = 15 * time.Second
	DefaultMySQLWriteTimeout     = 15 * time.Second
	DefaultMySQLMaxExecutionTime = 8 * time.Second
	DefaultMySQLLockWaitTimeout  = 10 * time.Second
	DefaultMySQLConnMaxIdleTime  = 5 * time.Minute
)

// DefaultMySQLOptions 返回带默认超时的连接池参数。
func DefaultMySQLOptions(maxOpenConns int, maxIdleConns int, connMaxLifetime time.Duration) MySQLOptions {
	return MySQLOptions{
		MaxOpenConns:     maxOpenConns,
		MaxIdleConns:     maxIdleConns,
		ConnMaxLifetime:  connMaxLifetime,
		ConnMaxIdleTime:  DefaultMySQLConnMaxIdleTime,
		QueryTimeout:     DefaultMySQLQueryTimeout,
		DialTimeout:      DefaultMySQLDialTimeout,
		ReadTimeout:      DefaultMySQLReadTimeout,
		WriteTimeout:     DefaultMySQLWriteTimeout,
		MaxExecutionTime: DefaultMySQLMaxExecutionTime,
		LockWaitTimeout:  DefaultMySQLLockWaitTimeout,
	}
}

// ApplyMySQLDSNTimeouts 把超时参数写进 DSN（timeout/readTimeout/writeTimeout 与会话变量
// max_execution_time、innodb_lock_wait_timeout）。DSN 里已显式出现的参数保持原样。
func ApplyMySQLDSNTimeouts(dsn string, o MySQLOptions) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	if cfg.Timeout == 0 && o.DialTimeout > 0 {
		cfg.Timeout = o.DialTimeout
	}
	if cfg.ReadTimeout == 0 && o.ReadTimeout > 0 {
		cfg.ReadTimeout = o.ReadTimeout
	}
	if cfg.WriteTimeout == 0 && o.WriteTimeout > 0 {
		cfg.WriteTimeout = o.WriteTimeout
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["max_execution_time"]; !ok && o.MaxExecutionTime > 0 {
		cfg.Params["max_execution_time"] = strconv.FormatInt(o.MaxExecutionTime.Milliseconds(), 10)
	}
	if _, ok := cfg.Params["innodb_lock_wait_timeout"]; !ok && o.LockWaitTimeout > 0 {
		secs := int64(o.LockWaitTimeout / time.Second)
		if secs < 1 {
			secs = 1
		}
		cfg.Params["innodb_lock_wait_timeout"] = strconv.FormatInt(secs, 10)
	}
	return cfg.FormatDSN(), nil
}

// ConfigurePool 设置 database/sql 连接池参数。
func ConfigurePool(sqlDB *sql.DB, o MySQLOptions) {
	sqlDB.SetMaxOpenConns(o.MaxOpenConns)
	sqlDB.SetMaxIdleConns(o.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(o.ConnMaxLifetime) //mysql 默认超时时间为 60*60*8=28800 SetConnMaxLifetime设置为小于数据库超时时间即可
	sqlDB.SetConnMaxIdleTime(o.ConnMaxIdleTime)
}

// NewSessionWithOptions 在已打开的 *sql.DB 上建 dbr 会话并套上连接池与单语句超时。
// 拆出来是为了让测试能用假驱动复现「池耗尽 + 慢查询」。
func NewSessionWithOptions(sqlDB *sql.DB, o MySQLOptions) *dbr.Session {
	ConfigurePool(sqlDB, o)
	conn := &dbr.Connection{DB: sqlDB, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	session := conn.NewSession(nil)
	if o.QueryTimeout > 0 {
		// dbr 对 Load/Exec 每条语句套 context.WithTimeout，覆盖等连接 + 执行；Rows()/Iterate() 不受影响。
		// Begin() 仍是 context.Background()，请求路径上的事务用 BeginTxTimeout。
		session.Timeout = o.QueryTimeout
	}
	return session
}

// NewMySQLWithOptions 创建一个带超时的 MySQL db
func NewMySQLWithOptions(addr string, o MySQLOptions) *dbr.Session {
	dsn, err := ApplyMySQLDSNTimeouts(addr, o)
	if err != nil {
		panic(err)
	}
	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		panic(err)
	}
	return NewSessionWithOptions(sqlDB, o)
}

// NewMySQL 创建一个MySQL db（默认超时见 DefaultMySQLOptions）
func NewMySQL(addr string, maxOpenConns int, maxIdleConns int, connMaxLifetime time.Duration) *dbr.Session {
	return NewMySQLWithOptions(addr, DefaultMySQLOptions(maxOpenConns, maxIdleConns, connMaxLifetime))
}

func Migration(sqlDir string, session *dbr.Session) error {
	migrations := &FileDirMigrationSource{
		Dir: sqlDir,
	}
	_, err := migrate.Exec(session.DB, "mysql", migrations, migrate.Up)
	if err != nil {
		return err
	}
	return nil
}

type byID []*migrate.Migration

func (b byID) Len() int           { return len(b) }
func (b byID) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byID) Less(i, j int) bool { return b[i].Less(b[j]) }

// FileDirMigrationSource 文件目录源 遇到目录进行递归获取
type FileDirMigrationSource struct {
	Dir string
}

// FindMigrations FindMigrations
func (f FileDirMigrationSource) FindMigrations() ([]*migrate.Migration, error) {
	filesystem := http.Dir(f.Dir)
	migrations := make([]*migrate.Migration, 0, 100)
	err := f.findMigrations(filesystem, &migrations)
	if err != nil {
		return nil, err
	}
	// Make sure migrations are sorted
	sort.Sort(byID(migrations))

	return migrations, nil
}

func (f FileDirMigrationSource) findMigrations(dir http.FileSystem, migrations *[]*migrate.Migration) error {

	file, err := dir.Open("/")
	if err != nil {
		return err
	}

	files, err := file.Readdir(0)
	if err != nil {
		return err
	}

	for _, info := range files {

		if strings.HasSuffix(info.Name(), ".sql") {
			file, err := dir.Open(info.Name())
			if err != nil {
				return fmt.Errorf("Error while opening %s: %s", info.Name(), err)
			}

			migration, err := migrate.ParseMigration(info.Name(), file)
			if err != nil {
				return fmt.Errorf("Error while parsing %s: %s", info.Name(), err)
			}
			*migrations = append(*migrations, migration)

		} else if info.IsDir() {
			err = f.findMigrations(http.Dir(fmt.Sprintf("%s/%s", f.Dir, info.Name())), migrations)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
