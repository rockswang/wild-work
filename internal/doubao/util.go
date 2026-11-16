package doubao

import (
	"crypto/rand"
	"time"
)

// randRead / timeNowUnixMilli 隔离 crypto/rand 与 time，便于测试替身。
// （本包不使用可注入的时钟，这两个间接层只为保持依赖面最小。）
func randRead(b []byte) (int, error) { return rand.Read(b) }

func timeNowUnixMilli() int64 { return time.Now().UnixMilli() }
