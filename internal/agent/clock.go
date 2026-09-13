package agent

import "time"

// Clock 抽象時間來源。接收時間、批次間隔與退避等待都經由它取得，
// 讓測試可以注入假時鐘，直接推進時間而不必真的等待。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock 為系統時鐘。
var RealClock Clock = realClock{}
