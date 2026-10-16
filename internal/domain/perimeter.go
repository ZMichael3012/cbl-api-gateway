package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Perimeter contract constants (CBL-7.2 FR-6, CBL-003 section 7).
const (
	// APIKeyCachePrefix sets the prefix of the Redis key with the API key check result.
	APIKeyCachePrefix = "apikey:v1:"
	// RateLimitPrefix sets the prefix of the Redis key with the rate limit counter.
	RateLimitPrefix = "rl:"
	// TokenIdentityPrefix sets the rate limit identity prefix for an unknown key.
	TokenIdentityPrefix = "token:"
	// RouteRoot sets the limit group for paths with no segment after /v1.
	RouteRoot = "root"
	// RouteOther sets the limit group for unrecognized path segments.
	RouteOther = "other"
	// routeMaxLength sets the maximum length of a limit group.
	routeMaxLength = 32
)

// merchantIDPattern repeats the platform merchant_id format (CBL-7.2):
// an arbitrary [A-Za-z0-9_-] string of up to 128 characters, not only a UUID
// (ops CLI and test stands use short identifiers like m-1).
var merchantIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// KeyStatus describes the outcome of reading the API key check cache.
type KeyStatus int

const (
	// KeyMiss means a cache miss: the decision about the key is made by the Billing API.
	KeyMiss KeyStatus = iota
	// KeyAllowed means a confirmed key with a known merchant.
	KeyAllowed
	// KeyDenied means a tombstone of a revoked key.
	KeyDenied
	// KeyInvalid means a corrupted cache value: it is treated as a miss.
	KeyInvalid
)

// AuthorizationToken extracts the Bearer token from Authorization header
// values: there must be exactly one header, the scheme is case-insensitive
// per RFC 7235, the token contains no spaces (CBL-7.2 rules).
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

// TokenHash returns the hex sha256 of the token: the perimeter check cache
// key and the rate limit identity for unknown keys.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// APIKeyCacheKey returns the Redis check cache key for a token hash.
func APIKeyCacheKey(tokenHash string) string {
	return APIKeyCachePrefix + tokenHash
}

// ParseKeyCacheValue parses a check cache value: deny rejects the request,
// ok:<merchant_id> confirms the merchant (merchant_id format as in 7.2),
// a miss and a corrupted value delegate the decision to the Billing API
// (the source of truth).
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

// RouteGroup normalizes the first path segment after the /v1 prefix into a
// limit group: lower case, [a-z0-9-]{1,32}; an empty segment is root,
// an unrecognized one is other.
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

// IdentityKey returns the rate limit identity: merchant_id for a known key
// or token:<hex> for a cache miss (natural boundaries and merchant NAT
// addresses do not penalize each other). The merchant_id format contains no
// colon, so the `rl:` key space remains consistent.
func IdentityKey(merchantID, tokenHash string) string {
	if merchantID != "" {
		return merchantID
	}
	return TokenIdentityPrefix + tokenHash
}

// RateLimitKey builds the rate limit counter key rl:{identity}:{route}:{window}.
func RateLimitKey(identity, route string, window int64) string {
	return RateLimitPrefix + identity + ":" + route + ":" + strconv.FormatInt(window, 10)
}
