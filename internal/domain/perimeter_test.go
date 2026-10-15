package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-api-gateway/internal/domain"
)

// TestAuthorizationToken проверяет строгий разбор Bearer-заголовка (правила 7.2).
func TestAuthorizationToken(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		token   string
		ok      bool
	}{
		{name: "валидный", headers: []string{"Bearer abc"}, token: "abc", ok: true},
		{name: "схема в нижнем регистре", headers: []string{"bearer abc"}, token: "abc", ok: true},
		{name: "лишние пробелы по краям", headers: []string{"  Bearer abc  "}, token: "abc", ok: true},
		{name: "отсутствует", headers: nil, ok: false},
		{name: "дубликат", headers: []string{"Bearer abc", "Bearer abc"}, ok: false},
		{name: "другая схема", headers: []string{"Basic abc"}, ok: false},
		{name: "без токена", headers: []string{"Bearer"}, ok: false},
		{name: "пустой токен", headers: []string{"Bearer   "}, ok: false},
		{name: "пробел в токене", headers: []string{"Bearer abc def"}, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, ok := domain.AuthorizationToken(test.headers)
			assert.Equal(t, test.ok, ok)
			assert.Equal(t, test.token, token)
		})
	}
}

// TestTokenHash проверяет известный вектор sha256 и нижний регистр.
func TestTokenHash(t *testing.T) {
	assert.Equal(t,
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		domain.TokenHash("test"))
	assert.Equal(t, 64, len(domain.TokenHash("cbl_v1_0123456789_secret")))
}

// TestAPIKeyCacheKey проверяет форму ключа кэша проверки (контракт 7.2 FR-6).
func TestAPIKeyCacheKey(t *testing.T) {
	assert.Equal(t, "apikey:v1:abc", domain.APIKeyCacheKey("abc"))
}

// TestParseKeyCacheValue проверяет матрицу значений кэша проверки.
func TestParseKeyCacheValue(t *testing.T) {
	uuidString := uuid.NewString()
	tests := []struct {
		name     string
		value    string
		present  bool
		status   domain.KeyStatus
		merchant string
	}{
		{name: "промах", status: domain.KeyMiss},
		{name: "deny", value: "deny", present: true, status: domain.KeyDenied},
		{name: "короткий идентификатор ops-CLI", value: "ok:m-1", present: true, status: domain.KeyAllowed, merchant: "m-1"},
		{name: "подчеркивание и дефис", value: "ok:M-1_a", present: true, status: domain.KeyAllowed, merchant: "M-1_a"},
		{name: "UUID", value: "ok:" + uuidString, present: true, status: domain.KeyAllowed, merchant: uuidString},
		{name: "двоеточие в идентификаторе", value: "ok:a:b", present: true, status: domain.KeyInvalid},
		{name: "пробел в идентификаторе", value: "ok:a b", present: true, status: domain.KeyInvalid},
		{name: "пустой идентификатор", value: "ok:", present: true, status: domain.KeyInvalid},
		{name: "слишком длинный идентификатор", value: "ok:" + strings.Repeat("a", 129), present: true, status: domain.KeyInvalid},
		{name: "неизвестное значение", value: "allow", present: true, status: domain.KeyInvalid},
		{name: "пустое значение", value: "", present: true, status: domain.KeyInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, merchant := domain.ParseKeyCacheValue(test.value, test.present)
			assert.Equal(t, test.status, status)
			assert.Equal(t, test.merchant, merchant)
		})
	}
}

// TestRouteGroup проверяет нормализацию первой группы пути.
func TestRouteGroup(t *testing.T) {
	tests := []struct {
		path  string
		group string
	}{
		{path: "/v1/invoices", group: "invoices"},
		{path: "/v1/invoices/01912f2f", group: "invoices"},
		{path: "/v1/Invoices", group: "invoices"},
		{path: "/v1/", group: "root"},
		{path: "/v1", group: "root"},
		{path: "/v1/webhook-endpoints", group: "webhook-endpoints"},
		{path: "/v1/ledger7", group: "ledger7"},
		{path: "/v1/../etc", group: "other"},
		{path: "/v1/inv oices", group: "other"},
		{path: "/v1/очки", group: "other"},
		{path: "/v1/" + strings.Repeat("a", 33), group: "other"},
		{path: "/v1/" + strings.Repeat("a", 32), group: strings.Repeat("a", 32)},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			assert.Equal(t, test.group, domain.RouteGroup(test.path))
		})
	}
}

// TestIdentityKey проверяет идентификатор лимита для известного и неизвестного ключа.
func TestIdentityKey(t *testing.T) {
	merchantID := uuid.NewString()
	assert.Equal(t, merchantID, domain.IdentityKey(merchantID, "hash"))
	assert.Equal(t, "token:hash", domain.IdentityKey("", "hash"))
}

// TestRateLimitKey проверяет форму ключа счетчика лимита (CBL-003 раздел 7).
func TestRateLimitKey(t *testing.T) {
	require.Equal(t, "rl:01912f2f-7d5e-7000-8000-000000000000:invoices:1700000000",
		domain.RateLimitKey("01912f2f-7d5e-7000-8000-000000000000", "invoices", 1700000000))
	assert.Equal(t, "rl:token:abc:payouts:1700000001",
		domain.RateLimitKey("token:abc", "payouts", 1700000001))
}
