package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/cbl/cbl-api-gateway/internal/domain"
)

// degradationLogInterval задает частоту журнала деградации: при 3000 RPS
// запись на каждый запрос превратила бы логи в поток, поэтому сбой зависимости
// фиксируется не чаще одного раза в интервал.
const degradationLogInterval = 5 * time.Second

// Counter считает обращения в окне лимита (Redis или локальный fallback).
type Counter interface {
	// Incr увеличивает счетчик ключа окна и возвращает значение после инкремента.
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// KeyPeeker читает кэш проверки API-ключей apikey:v1:*, который заполняет
// Billing API (контракт 7.2 FR-6).
type KeyPeeker interface {
	// Get возвращает значение ключа; found=false при промахе.
	Get(ctx context.Context, key string) (string, bool, error)
}

// KeyCheck описывает исход контроля ключа на периметре.
type KeyCheck struct {
	// Deny запрещает запрос без обращения к Billing API (tombstone отзыва).
	Deny bool
	// MerchantID содержит владельца ключа при подтвержденном кэше.
	MerchantID string
}

// LimitDecision описывает решение лимита запроса.
type LimitDecision struct {
	// Allowed разрешает проксирование запроса.
	Allowed bool
	// RetryAfter задает остаток окна для заголовка Retry-After при отказе.
	RetryAfter time.Duration
}

// Perimeter применяет правила периметра: контроль ключа по кэшу Billing API и
// лимит на мерчанта в окне фиксированной длительности.
type Perimeter struct {
	keys    KeyPeeker
	counter Counter
	log     *zap.Logger
	limit   int64
	window  time.Duration

	logMu    sync.Mutex
	lastWarn time.Time
}

// NewPerimeter собирает правила периметра; лимит и окно обязаны быть
// положительными (невалидная конфигурация - отказ старта, а не тихий пропуск).
// Окно ключей контракта CBL-003 - одна секунда (значение задает main).
func NewPerimeter(keys KeyPeeker, counter Counter, log *zap.Logger, limit int64, window time.Duration) (*Perimeter, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("rate limit must be positive: %d", limit)
	}
	if window <= 0 {
		return nil, fmt.Errorf("rate window must be positive: %s", window)
	}
	if counter == nil {
		return nil, errors.New("counter is required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Perimeter{keys: keys, counter: counter, log: log, limit: limit, window: window}, nil
}

// CheckKey читает кэш проверки: deny запрещает запрос, ok подтверждает
// мерчанта, промах, ошибка Redis и испорченное значение делегируют решение
// Billing API (периметр не выносит авторитетных решений о неизвестном ключе).
func (p *Perimeter) CheckKey(ctx context.Context, tokenHash string) KeyCheck {
	if p.keys == nil {
		return KeyCheck{}
	}
	value, present, err := p.keys.Get(ctx, domain.APIKeyCacheKey(tokenHash))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Клиент оборвал запрос: это не сбой зависимости.
			return KeyCheck{}
		}
		p.warnDegraded("api key cache read failed, request is delegated to billing api", err)
		return KeyCheck{}
	}
	status, merchantID := domain.ParseKeyCacheValue(value, present)
	switch status {
	case domain.KeyAllowed:
		return KeyCheck{MerchantID: merchantID}
	case domain.KeyDenied:
		return KeyCheck{Deny: true}
	case domain.KeyInvalid:
		p.warnDegraded("api key cache value is invalid, request is delegated to billing api", nil)
	}
	return KeyCheck{}
}

// Allow считает запрос в окне фиксированной длительности и решает, пропускать
// ли его; при недоступности обоих счетчиков запрос пропускается (доступность
// периметра важнее, авторитетная проверка ключа остается за Billing API).
func (p *Perimeter) Allow(ctx context.Context, identity, route string, now time.Time) LimitDecision {
	windowIndex := now.UnixNano() / int64(p.window)
	key := domain.RateLimitKey(identity, route, windowIndex)
	count, err := p.counter.Incr(ctx, key, 2*p.window)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return LimitDecision{Allowed: true}
		}
		p.warnDegraded("rate counter failed, request is allowed without limit", err)
		return LimitDecision{Allowed: true}
	}
	if count <= p.limit {
		return LimitDecision{Allowed: true}
	}
	windowEnd := time.Unix(0, (windowIndex+1)*int64(p.window))
	return LimitDecision{Allowed: false, RetryAfter: windowEnd.Sub(now)}
}

// warnDegraded пишет журнал деградации не чаще degradationLogInterval.
func (p *Perimeter) warnDegraded(message string, err error) {
	now := time.Now()
	p.logMu.Lock()
	skip := now.Sub(p.lastWarn) < degradationLogInterval
	if !skip {
		p.lastWarn = now
	}
	p.logMu.Unlock()
	if skip {
		return
	}
	if err != nil {
		p.log.Warn(message, zap.Error(err))
		return
	}
	p.log.Warn(message)
}
