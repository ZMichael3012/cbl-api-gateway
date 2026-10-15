package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// degradeWindow задает окно локального предохранителя: после первой ошибки
// Redis запросы degradeWindow не обращаются к Redis вовсе, а сразу уходят на
// локальный лимитер. Без предохранителя каждый запрос платил бы таймауты
// клиента (до ~1 с на операцию), а при 3000 RPS это тысячи горутин.
const degradeWindow = time.Second

// ErrDegraded означает, что Redis помечен недоступным предохранителем.
var ErrDegraded = errors.New("redis is degraded")

// redisIncrScript атомарно увеличивает счетчик окна и выставляет TTL, если его
// нет (ключ мог быть создан извне без TTL): решение лимита не теряет
// атомарность, счетчики не утекают.
var redisIncrScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if redis.call('PTTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count`)

// RedisStore обслуживает Redis периметра: кэш проверки ключей и счетчики
// лимитов.
type RedisStore struct {
	client *redis.Client

	mu            sync.Mutex
	degradedUntil time.Time
}

// NewRedisStore создает клиент с короткими таймаутами горячего пути: первая
// ошибка Redis стоит до ~1 с (две попытки с таймаутами 200/300 мс), после чего
// предохранитель на degradeWindow переключает запросы на локальный лимитер.
func NewRedisStore(addr string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           200 * time.Millisecond,
		ReadTimeout:           300 * time.Millisecond,
		WriteTimeout:          300 * time.Millisecond,
		MaxRetries:            1,
		ContextTimeoutEnabled: true,
	})
	return &RedisStore{client: client}, nil
}

// Ping проверяет доступность Redis.
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Close закрывает соединение с Redis.
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// Get читает значение ключа; found=false при промахе.
func (s *RedisStore) Get(ctx context.Context, key string) (string, bool, error) {
	if err := s.gate(); err != nil {
		return "", false, err
	}
	value, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		s.trip(ctx, err)
		return "", false, err
	}
	return value, true, nil
}

// Incr атомарно увеличивает счетчик окна и возвращает значение после
// инкремента; TTL выставляется, если его нет, счетчики не утекают.
func (s *RedisStore) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if err := s.gate(); err != nil {
		return 0, err
	}
	milliseconds := ttl.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	count, err := redisIncrScript.Run(ctx, s.client, []string{key}, milliseconds).Int64()
	if err != nil {
		s.trip(ctx, err)
		return 0, fmt.Errorf("incr window: %w", err)
	}
	return count, nil
}

// gate проверяет предохранитель: в окне деградации обращения к Redis нет.
func (s *RedisStore) gate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.degradedUntil) {
		return ErrDegraded
	}
	return nil
}

// trip помечает Redis недоступным на degradeWindow; отмена клиента не
// считается сбоем зависимости и предохранитель не взводит.
func (s *RedisStore) trip(ctx context.Context, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	s.mu.Lock()
	s.degradedUntil = time.Now().Add(degradeWindow)
	s.mu.Unlock()
}
