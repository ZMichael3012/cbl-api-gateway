package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// TestFallbackCounterPrimary verifies the normal path through the primary counter.
func TestFallbackCounterPrimary(t *testing.T) {
	primary := &fakeCounter{}
	fallback := &fakeCounter{}
	counter := usecase.NewFallbackCounter(primary, fallback, nil)
	count, err := counter.Incr(context.Background(), "key", time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	assert.Equal(t, 1, len(primary.keys))
	assert.Empty(t, fallback.keys)
}

// TestFallbackCounterDegrade verifies switching to the local counter.
func TestFallbackCounterDegrade(t *testing.T) {
	primary := &fakeCounter{err: errors.New("redis down")}
	fallback := &fakeCounter{}
	counter := usecase.NewFallbackCounter(primary, fallback, nil)
	count, err := counter.Incr(context.Background(), "key", time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the local counter makes the decision")
	assert.Equal(t, 1, len(fallback.keys))
}

// TestFallbackCounterEdges verifies degenerate dependency combinations.
func TestFallbackCounterEdges(t *testing.T) {
	fallback := &fakeCounter{}
	assert.Equal(t, usecase.Counter(fallback), usecase.NewFallbackCounter(nil, fallback, nil))
	primary := &fakeCounter{}
	assert.Equal(t, usecase.Counter(primary), usecase.NewFallbackCounter(primary, nil, nil))
}
