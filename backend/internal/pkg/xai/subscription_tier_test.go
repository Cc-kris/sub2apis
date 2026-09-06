package xai

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func testJWTWithTier(t *testing.T, tier any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"tier": tier})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestSubscriptionTierFromJWTMapsNumericAndStringClaims(t *testing.T) {
	require.Equal(t, "supergrok_heavy", SubscriptionTierFromJWT(testJWTWithTier(t, 5)))
	require.Equal(t, "free", SubscriptionTierFromJWT(testJWTWithTier(t, "FREE")))
	require.Equal(t, "supergrok_plus", SubscriptionTierFromJWT(testJWTWithTier(t, "7")))
	require.Empty(t, SubscriptionTierFromJWT(testJWTWithTier(t, -1)))
	require.Empty(t, SubscriptionTierFromJWT("not-a-jwt"))
}

func TestNormalizeSubscriptionTier(t *testing.T) {
	require.Equal(t, "supergrok", NormalizeSubscriptionTier(" SuperGrok Pro "))
	require.Equal(t, "free", NormalizeSubscriptionTier("grok-basic"))
	require.Equal(t, "x_premium_plus", NormalizeSubscriptionTier("X Premium+"))
}
