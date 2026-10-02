package session

import "time"

// Clock 返回当前时刻，可注入用于测试。
type Clock interface {
	Now() time.Time
}

// SystemClock 使用真实系统时间。
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock 是可手动推进的单调时钟，用于测试。
type ManualClock struct {
	now time.Time
}

func NewManualClock(start time.Time) *ManualClock { return &ManualClock{now: start} }

func (c *ManualClock) Now() time.Time { return c.now }

// Advance 将时钟向前推进 d。
func (c *ManualClock) Advance(d time.Duration) { c.now = c.now.Add(d) }
