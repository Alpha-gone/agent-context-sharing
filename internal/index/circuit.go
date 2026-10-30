package index

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/diagnostic"
)

const maxProviderRetryAfter = 24 * time.Hour
const providerProbeInput = "index provider recovery probe"

// cycleMu를 가진 회차만 차단 상태를 읽고 갱신한다. 질의 임베딩과는 공유하지 않는다.
type providerCircuit struct {
	failures int
	retryAt  time.Time
}

func (worker *Worker) providerTime() time.Time {
	if worker.providerNow != nil {
		return worker.providerNow()
	}
	return time.Now()
}

func providerWideFailure(ctx context.Context, status uint16, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	if status == 401 || status == 403 || status == 408 || status == 429 || (status >= 500 && status < 600) {
		return true
	}
	if failure, ok := errors.AsType[ProviderError](err); ok {
		return failure.Retryable
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func (worker *Worker) blockProvider(ctx context.Context, status uint16, err error) {
	state := &worker.providerCircuit
	state.failures = min(state.failures+1, 5)
	ceiling := min(time.Minute*time.Duration(1<<(state.failures-1)), 15*time.Minute)
	delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))
	if failure, ok := errors.AsType[ProviderError](err); ok {
		delay = max(delay, min(failure.RetryAfter, maxProviderRetryAfter))
	}
	state.retryAt = worker.providerTime().Add(delay)
	worker.logger.WarnContext(ctx, "제공자 장애로 색인 작업 확보 중단", "reason_code", "provider_blocked", "provider_status", status, "retry_after_seconds", int64(delay/time.Second))
	diagnostic.Observe(worker.diagnostics, ctx, "WARN", "provider", "pending", "provider_blocked", false, diagnostic.Fields{Index: diagnostic.IndexFields{ProviderStatus: status}})
}

// 재개 확인은 큐 확보 없이 수행해 다른 작업의 시도 횟수를 소진하지 않는다.
func (worker *Worker) providerReady(ctx context.Context) (bool, error) {
	if worker.providerCircuit.retryAt.IsZero() {
		return true, nil
	}
	if worker.providerTime().Before(worker.providerCircuit.retryAt) {
		return false, nil
	}
	_, status, err := worker.embedResponse(ctx, providerProbeInput, true)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		worker.blockProvider(ctx, status, err)
		return false, nil
	}
	worker.providerCircuit = providerCircuit{}
	worker.logger.InfoContext(ctx, "제공자 회복으로 색인 작업 확보 재개", "reason_code", "provider_resumed", "provider_status", status)
	diagnostic.Observe(worker.diagnostics, ctx, "INFO", "provider", "completed", "provider_resumed", false, diagnostic.Fields{Index: diagnostic.IndexFields{ProviderStatus: status}})
	return true, nil
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	digits := true
	for _, char := range raw {
		if char < '0' || char > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || seconds >= uint64(maxProviderRetryAfter/time.Second) {
			return maxProviderRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(raw); err == nil {
		return min(max(date.Sub(now), 0), maxProviderRetryAfter)
	}
	return 0
}
