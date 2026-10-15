package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Константы контрактов периметра (CBL-7.2 FR-6, CBL-003 раздел 7).
const (
	// APIKeyCachePrefix задает префикс ключа Redis с результатом проверки API-ключа.
	APIKeyCachePrefix = "apikey:v1:"
	// RateLimitPrefix задает префикс ключа Redis со счетчиком лимита.
	RateLimitPrefix = "rl:"
	// TokenIdentityPrefix задает префикс идентификатора лимита для неизвестного ключа.
	TokenIdentityPrefix = "token:"
	// RouteRoot задает группу лимита для путей без сегмента после /v1.
	RouteRoot = "root"
	// RouteOther задает группу лимита для нераспознанных сегментов пути.
	RouteOther = "other"
	// routeMaxLength задает предельную длину группы лимита.
	routeMaxLength = 32
)

// merchantIDPattern повторяет формат merchant_id платформы (CBL-7.2):
// произвольная строка [A-Za-z0-9_-] длиной до 128 символов, а не только UUID
// (ops-CLI и тестовые стенды используют короткие идентификаторы вида m-1).
var merchantIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// KeyStatus описывает исход чтения кэша проверки API-ключа.
type KeyStatus int

const (
	// KeyMiss означает промах кэша: решение о ключе принимает Billing API.
	KeyMiss KeyStatus = iota
	// KeyAllowed означает подтвержденный ключ с известным мерчантом.
	KeyAllowed
	// KeyDenied означает tombstone отозванного ключа.
	KeyDenied
	// KeyInvalid означает испорченное значение кэша: трактуется как промах.
	KeyInvalid
)

// AuthorizationToken извлекает Bearer-токен из значений заголовка
// Authorization: заголовок должен быть ровно один, схема регистронезависима
// по RFC 7235, токен без пробелов (правила CBL-7.2).
func AuthorizationToken(headers []string) (string, bool) {
	if len(headers) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(strings.TrimSpace(headers[0]), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// TokenHash возвращает hex sha256 токена: ключ кэша проверки на периметре и
// идентификатор лимита для неизвестных ключей.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// APIKeyCacheKey возвращает ключ Redis кэша проверки для хэша токена.
func APIKeyCacheKey(tokenHash string) string {
	return APIKeyCachePrefix + tokenHash
}

// ParseKeyCacheValue разбирает значение кэша проверки: deny запрещает запрос,
// ok:<merchant_id> подтверждает мерчанта (формат merchant_id как в 7.2),
// промах и испорченное значение делегируют решение Billing API (источник истины).
func ParseKeyCacheValue(value string, present bool) (KeyStatus, string) {
	if !present {
		return KeyMiss, ""
	}
	if value == "deny" {
		return KeyDenied, ""
	}
	merchantID, ok := strings.CutPrefix(value, "ok:")
	if !ok {
		return KeyInvalid, ""
	}
	if !merchantIDPattern.MatchString(merchantID) {
		return KeyInvalid, ""
	}
	return KeyAllowed, merchantID
}

// RouteGroup нормализует первый сегмент пути после префикса /v1 в группу
// лимита: нижний регистр, [a-z0-9-]{1,32}; пустой сегмент - root,
// нераспознанный - other.
func RouteGroup(path string) string {
	trimmed := strings.TrimPrefix(path, "/v1")
	trimmed = strings.TrimPrefix(trimmed, "/")
	segment, _, _ := strings.Cut(trimmed, "/")
	if segment == "" {
		return RouteRoot
	}
	segment = strings.ToLower(segment)
	if len(segment) > routeMaxLength {
		return RouteOther
	}
	for _, symbol := range segment {
		allowed := (symbol >= 'a' && symbol <= 'z') || (symbol >= '0' && symbol <= '9') || symbol == '-'
		if !allowed {
			return RouteOther
		}
	}
	return segment
}

// IdentityKey возвращает идентификатор лимита: merchant_id при известном
// ключе или token:<hex> для промаха кэша (натуральные границы и NAT-адреса
// мерчантов не штрафуют друг друга). Формат merchant_id не содержит двоеточия,
// поэтому пространство ключей `rl:` остается непротиворечивым.
func IdentityKey(merchantID, tokenHash string) string {
	if merchantID != "" {
		return merchantID
	}
	return TokenIdentityPrefix + tokenHash
}

// RateLimitKey собирает ключ счетчика лимита rl:{identity}:{route}:{window}.
func RateLimitKey(identity, route string, window int64) string {
	return RateLimitPrefix + identity + ":" + route + ":" + strconv.FormatInt(window, 10)
}
