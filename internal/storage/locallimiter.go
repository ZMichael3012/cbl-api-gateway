package storage

import (
	"context"
	"sync"
	"time"
)

// sweepInterval задает периодичность уборки истекших записей (каждые N
// инкрементов), чтобы карта не росла бесконечно при потоке уникальных ключей.
const sweepInterval = 1024

// LocalLimiter считает окна в памяти процесса: fallback при недоступности
// Redis и локальный режим без REDIS_ADDR; при нескольких репликах эффективный
// лимит умножается на их число (trade-off периметра).
type LocalLimiter struct {
	mu     sync.Mutex
	counts map[string]localWindow
	ops    int
}

// localWindow хранит счетчик и момент истечения записи окна.
type localWindow struct {
	count     int64
	expiresAt time.Time
}

// NewLocalLimiter создает локальный лимитер.
func NewLocalLimiter() *LocalLimiter {
	return &LocalLimiter{counts: make(map[string]localWindow)}
}

// Incr увеличивает счетчик ключа, живущий до ttl (для ключей окна ttl
// покрывает окно с запасом); истекшая запись переиспользуется с нуля.
func (l *LocalLimiter) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.counts[key]
	if !ok || !entry.expiresAt.After(now) {
		entry = localWindow{expiresAt: now.Add(ttl)}
	}
	entry.count++
	l.counts[key] = entry
	l.ops++
	if l.ops%sweepInterval == 0 {
		l.sweep(now)
	}
	return entry.count, nil
}

// sweep удаляет истекшие записи; вызывается редко, стоимость амортизирована.
func (l *LocalLimiter) sweep(now time.Time) {
	for key, entry := range l.counts {
		if !entry.expiresAt.After(now) {
			delete(l.counts, key)
		}
	}
}
