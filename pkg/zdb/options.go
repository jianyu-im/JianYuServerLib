/**
 * @Author : nopsky
 * @Email : cnnopsky@gmail.com
 * @Date : 2021/3/18 14:03
 */
package zdb

import (
	"time"

	"github.com/jianyu-im/JianYuServerLib/pkg/db"
)

type Options struct {
	Dns          string
	MaxIdleConns int
	MaxOpenConns int
	Mode         string

	// OrmMaxOpenConns/OrmMaxIdleConns 单独限制 zdb 这个池；不写则沿用 MaxOpenConns/MaxIdleConns。
	// 注意 zdb 与 dbr 主库是两个独立的池，按同一个 maxOpenConns 各开一份，实际连接上限是两者之和。
	OrmMaxOpenConns int
	OrmMaxIdleConns int

	// 超时（<0 关闭该层；0/不写取 pkg/db 默认值），与 dbr 主库共用同一组配置键
	ConnMaxLifetime  time.Duration
	ConnMaxIdleTime  time.Duration
	QueryTimeout     time.Duration
	DialTimeout      time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxExecutionTime time.Duration
	LockWaitTimeout  time.Duration
}

// mysqlOptions 合并默认值后的连接池/超时参数
func (o *Options) mysqlOptions() db.MySQLOptions {
	maxOpen, maxIdle := o.MaxOpenConns, o.MaxIdleConns
	if o.OrmMaxOpenConns > 0 {
		maxOpen = o.OrmMaxOpenConns
	}
	if o.OrmMaxIdleConns > 0 {
		maxIdle = o.OrmMaxIdleConns
	}
	lifetime := o.ConnMaxLifetime
	if lifetime == 0 {
		lifetime = time.Hour
	}
	m := db.DefaultMySQLOptions(maxOpen, maxIdle, lifetime)
	pick := func(dst *time.Duration, v time.Duration) {
		if v != 0 {
			*dst = v
		}
	}
	pick(&m.ConnMaxIdleTime, o.ConnMaxIdleTime)
	pick(&m.QueryTimeout, o.QueryTimeout)
	pick(&m.DialTimeout, o.DialTimeout)
	pick(&m.ReadTimeout, o.ReadTimeout)
	pick(&m.WriteTimeout, o.WriteTimeout)
	pick(&m.MaxExecutionTime, o.MaxExecutionTime)
	pick(&m.LockWaitTimeout, o.LockWaitTimeout)
	return m
}

func newOptions() *Options {
	return &Options{}
}
