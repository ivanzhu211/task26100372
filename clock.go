package sessionmgr

import "time"

// Clock 提供单调时间来源，可注入假时钟用于测试。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
