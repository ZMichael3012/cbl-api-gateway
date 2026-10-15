package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// fakePeeker управляет значением кэша проверки ключей.
type fakePeeker struct {
	value   string
	present bool
	err     error
	calls   []string
}

// Get возвращает заданное значение и запоминает ключи.
func (f *fakePeeker) Get(_ context.Context, key string) (string, bool, error) {
	f.calls = append(f.calls, key)
	return f.value, f.present, f.err
}

// fakeCounter управляет счетчиком лимита: считает по ключам окна, как Redis.
type fakeCounter struct {
	counts map[string]int64
	err    error
	keys   []string
	ttls   []time.Duration
}

// Incr увеличивает счетчик ключа и запоминает параметры вызова.
func (f *fakeCounter) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.counts == nil {
		f.counts = make(map[string]int64)
	}
	f.counts[key]++
	f.keys = append(f.keys, key)
	f.ttls = append(f.ttls, ttl)
	return f.counts[key], nil
}

// newPerimeter собирает сервис с фейками.
func newPerimeter(t *testing.T, peeker *fakePeeker, counter *fakeCounter, limit int64) *usecase.Perimeter {
	t.Helper()
	perimeter, err := usecase.NewPerimeter(peeker, counter, nil, limit, time.Second)
	require.NoError(t, err)
	return perimeter
}

// TestNewPerimeterValidation проверяет fail-fast конфигурации.
func TestNewPerimeterValidation(t *testing.T) {
	counter := &fakeCounter{}
	_, err := usecase.NewPerimeter(nil, counter, nil, 0, time.Second)
	require.Error(t, err, "нулевой лимит запрещен")
	_, err = usecase.NewPerimeter(nil, counter, nil, 1, 0)
	require.Error(t, err, "нулевое окно запрещено")
	_, err = usecase.NewPerimeter(nil, nil, nil, 1, time.Second)
	require.Error(t, err, "без счетчика сборка невозможна")
	_, err = usecase.NewPerimeter(nil, counter, nil, 1, time.Second)
	require.NoError(t, err, "периметр без Redis допустим (только делегирование)")
}

// TestCheckKeyAllowed проверяет подтверждение владельца ключа.
func TestCheckKeyAllowed(t *testing.T) {
	merchantID := "01912f2f-6f82-7d5e-8e04-4d4b5b6a1234"
	peeker := &fakePeeker{value: "ok:" + merchantID, present: true}
	check := newPerimeter(t, peeker, &fakeCounter{}, 10).CheckKey(context.Background(), "hash")
	assert.False(t, check.Deny)
	assert.Equal(t, merchantID, check.MerchantID)
	assert.Equal(t, []string{domain.APIKeyCacheKey("hash")}, peeker.calls)
}

// TestCheckKeyDenied проверяет немедленный запрет по tombstone.
func TestCheckKeyDenied(t *testing.T) {
	peeker := &fakePeeker{value: "deny", present: true}
	check := newPerimeter(t, peeker, &fakeCounter{}, 10).CheckKey(context.Background(), "hash")
	assert.True(t, check.Deny)
	assert.Empty(t, check.MerchantID)
}

// TestCheckKeyDelegates проверяет делегирование BFF при промахе, ошибке и
// испорченном значении кэша.
func TestCheckKeyDelegates(t *testing.T) {
	miss := newPerimeter(t, &fakePeeker{}, &fakeCounter{}, 10)
	check := miss.CheckKey(context.Background(), "hash")
	assert.False(t, check.Deny)
	assert.Empty(t, check.MerchantID)

	failed := newPerimeter(t, &fakePeeker{err: errors.New("redis down")}, &fakeCounter{}, 10)
	check = failed.CheckKey(context.Background(), "hash")
	assert.False(t, check.Deny)
	assert.Empty(t, check.MerchantID)

	poisoned := newPerimeter(t, &fakePeeker{value: "ok:bad id", present: true}, &fakeCounter{}, 10)
	check = poisoned.CheckKey(context.Background(), "hash")
	assert.False(t, check.Deny)
	assert.Empty(t, check.MerchantID)

	noRedis, err := usecase.NewPerimeter(nil, &fakeCounter{}, nil, 10, time.Second)
	require.NoError(t, err)
	check = noRedis.CheckKey(context.Background(), "hash")
	assert.False(t, check.Deny)
	assert.Empty(t, check.MerchantID)
}

// TestAllowWithinLimit проверяет пропуск до лимита включительно.
func TestAllowWithinLimit(t *testing.T) {
	counter := &fakeCounter{}
	perimeter := newPerimeter(t, nil, counter, 2)
	now := time.Unix(1700000000, 500*int64(time.Millisecond))
	for request := 1; request <= 2; request++ {
		decision := perimeter.Allow(context.Background(), "token:abc", "invoices", now)
		assert.True(t, decision.Allowed, "запрос %d в пределах лимита", request)
	}
	assert.Equal(t, "rl:token:abc:invoices:1700000000", counter.keys[0], "ключ контракта CBL-003")
	assert.Equal(t, 2*time.Second, counter.ttls[0], "TTL покрывает окно с запасом")
}

// TestAllowOverLimit проверяет отказ и Retry-After на остаток окна.
func TestAllowOverLimit(t *testing.T) {
	counter := &fakeCounter{}
	perimeter := newPerimeter(t, nil, counter, 1)
	now := time.Unix(1700000000, 400*int64(time.Millisecond))
	require.True(t, perimeter.Allow(context.Background(), "m", "invoices", now).Allowed)
	decision := perimeter.Allow(context.Background(), "m", "invoices", now.Add(100*time.Millisecond))
	assert.False(t, decision.Allowed)
	assert.Equal(t, 500*time.Millisecond, decision.RetryAfter, "остаток окна до конца секунды")
	assert.GreaterOrEqual(t, decision.RetryAfter, time.Second/2)
}

// TestAllowWindowRollover проверяет начало нового окна на границе секунды.
func TestAllowWindowRollover(t *testing.T) {
	counter := &fakeCounter{}
	perimeter := newPerimeter(t, nil, counter, 1)
	first := time.Unix(1700000000, 0)
	second := time.Unix(1700000001, 0)
	require.True(t, perimeter.Allow(context.Background(), "m", "invoices", first).Allowed)
	decision := perimeter.Allow(context.Background(), "m", "invoices", second)
	assert.True(t, decision.Allowed, "новое окно начинает счет заново")
	assert.Len(t, counter.keys, 2)
	assert.NotEqual(t, counter.keys[0], counter.keys[1], "окна различаются номером секунды")
}

// TestCheckKeyHandlesCanceledContext проверяет, что обрыв клиента не считается
// сбоем зависимости и делегируется BFF без шумного журнала.
func TestCheckKeyHandlesCanceledContext(t *testing.T) {
	peeker := &fakePeeker{err: context.Canceled}
	check := newPerimeter(t, peeker, &fakeCounter{}, 10).CheckKey(context.Background(), "hash")
	assert.False(t, check.Deny)
	assert.Empty(t, check.MerchantID)
}

// TestAllowCanceledCounterPassesWithoutLimit проверяет пропуск запроса при
// отмене клиента: решение о ключе уже принято, лимитировать нечего.
func TestAllowCanceledCounterPassesWithoutLimit(t *testing.T) {
	counter := &fakeCounter{err: context.Canceled}
	decision := newPerimeter(t, nil, counter, 1).Allow(context.Background(), "m", "invoices", time.Now())
	assert.True(t, decision.Allowed)
}

// TestAllowCounterFailure проверяет fail-open при отказе обоих счетчиков.
func TestAllowCounterFailure(t *testing.T) {
	counter := &fakeCounter{err: errors.New("counter unavailable")}
	perimeter := newPerimeter(t, nil, counter, 1)
	decision := perimeter.Allow(context.Background(), "m", "invoices", time.Now())
	assert.True(t, decision.Allowed, "доступность важнее точности, ключ проверит BFF")
}

// TestAllowRetryAfterMinimum проверяет малый остаток окна: заголовок
// округляется middleware до целой секунды, решение несет точный остаток.
func TestAllowRetryAfterMinimum(t *testing.T) {
	counter := &fakeCounter{}
	perimeter := newPerimeter(t, nil, counter, 1)
	now := time.Unix(1700000000, 999*int64(time.Millisecond))
	require.True(t, perimeter.Allow(context.Background(), "m", "invoices", now).Allowed)
	decision := perimeter.Allow(context.Background(), "m", "invoices", now.Add(time.Nanosecond))
	assert.False(t, decision.Allowed)
	assert.Greater(t, decision.RetryAfter, time.Duration(0))
	assert.Less(t, decision.RetryAfter, time.Second)
}

// TestIdentitySwitchAcrossWindows проверяет независимость корзин промаха и
// подтвержденного мерчанта (всплеск перехода не ослабляет защиту дальше 2x).
func TestIdentitySwitchAcrossWindows(t *testing.T) {
	counter := &fakeCounter{}
	perimeter := newPerimeter(t, nil, counter, 1)
	now := time.Unix(1700000000, 0)
	require.True(t, perimeter.Allow(context.Background(), domain.IdentityKey("", "hash"), "invoices", now).Allowed)
	assert.False(t, perimeter.Allow(context.Background(), domain.IdentityKey("", "hash"), "invoices", now).Allowed)
	merchant := domain.IdentityKey("01912f2f-6f82-7d5e-8e04-4d4b5b6a1234", "hash")
	assert.True(t, perimeter.Allow(context.Background(), merchant, "invoices", now).Allowed)
	assert.False(t, perimeter.Allow(context.Background(), merchant, "invoices", now).Allowed)
	assert.True(t, strings.HasSuffix(counter.keys[len(counter.keys)-1], ":1700000000"), "окно выровнено по секунде")
}
