package service

import (
	"math/rand"
	"sync"
	"time"

	"claude2api/internal/config"
)

// 全局请求排队门闸：串行化所有上游请求的发起时刻，保证相邻两次请求的开始时间
// 间隔为随机 [min, max] 秒（默认 1-5 秒，config.yaml 可配），避免请求过于频繁被上游封禁。
// min 或 max 配置 <= 0 时禁用排队；max < min 时按 max = min 处理。
var (
	reqGateMu    sync.Mutex
	lastGatePass time.Time
	gateRand     = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// waitRequestGate 在每次上游请求开始前调用（Dispatcher.Complete 入口），
// 串行阻塞至距上一次放行时刻满随机间隔后放行，并记录本次放行时间。
func waitRequestGate() {
	s := config.Get()
	minS, maxS := s.RequestQueueMinSeconds, s.RequestQueueMaxSeconds
	if minS <= 0 || maxS <= 0 {
		return
	}
	if maxS < minS {
		maxS = minS
	}
	reqGateMu.Lock()
	defer reqGateMu.Unlock()
	wait := time.Duration(minS+gateRand.Intn(maxS-minS+1)) * time.Second
	if !lastGatePass.IsZero() {
		if elapsed := time.Since(lastGatePass); elapsed < wait {
			time.Sleep(wait - elapsed)
		}
	}
	lastGatePass = time.Now()
}
