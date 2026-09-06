package xai

import (
	"encoding/json"
	"strconv"
	"strings"
)

// MapJWTSubscriptionTier maps the numeric xAI JWT tier claim to a stable
// identifier used by the account usage and scheduling paths.
func MapJWTSubscriptionTier(tier uint64) string {
	switch tier {
	case 0:
		return "free"
	case 1:
		return "supergrok"
	case 2:
		return "x_basic"
	case 3:
		return "x_premium"
	case 4:
		return "x_premium_plus"
	case 5:
		return "supergrok_heavy"
	case 6:
		return "supergrok_lite"
	case 7:
		return "supergrok_plus"
	default:
		return strconv.FormatUint(tier, 10)
	}
}

// NormalizeSubscriptionTier canonicalizes JWT and provider display names.
func NormalizeSubscriptionTier(raw string) string {
	t := strings.ToLower(strings.TrimSpace(raw))
	t = strings.ReplaceAll(t, "-", "_")
	t = strings.Join(strings.Fields(t), "_")
	switch t {
	case "free", "grok_free", "grokfree", "free_tier", "freetier", "grok_basic", "grokbasic", "basic":
		return "free"
	case "supergrok", "grokpro", "supergrok_pro", "supergrokpro":
		return "supergrok"
	case "supergrok_lite", "supergroklite":
		return "supergrok_lite"
	case "supergrok_heavy", "supergrokheavy":
		return "supergrok_heavy"
	case "supergrok_plus", "supergrokplus":
		return "supergrok_plus"
	case "x_basic", "xbasic":
		return "x_basic"
	case "x_premium", "xpremium":
		return "x_premium"
	case "x_premium_plus", "xpremiumplus", "x_premium+":
		return "x_premium_plus"
	default:
		return t
	}
}

// SubscriptionTierFromJWT decodes only the JWT payload (no signature
// verification) and returns the normalized tier claim. The token is already
// authenticated by the OAuth exchange; this helper never treats the claim as
// an authentication decision.
func SubscriptionTierFromJWT(token string) string {
	claims := DecodeJWTClaims(token)
	if claims == nil {
		return ""
	}
	raw, ok := claims["tier"]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case float64:
		if value < 0 {
			return ""
		}
		return MapJWTSubscriptionTier(uint64(value))
	case json.Number:
		n, err := value.Int64()
		if err != nil || n < 0 {
			return NormalizeSubscriptionTier(value.String())
		}
		return MapJWTSubscriptionTier(uint64(n))
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return ""
		}
		if n, err := strconv.ParseUint(value, 10, 64); err == nil {
			return MapJWTSubscriptionTier(n)
		}
		return NormalizeSubscriptionTier(value)
	default:
		return ""
	}
}
