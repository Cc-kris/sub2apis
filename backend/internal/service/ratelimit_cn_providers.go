package service

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CNProviderRateLimitWindow captures provider rate-limit headers without
// exposing credentials or response bodies.
type CNProviderRateLimitWindow struct {
	Limit     *int64     `json:"limit,omitempty"`
	Remaining *int64     `json:"remaining,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// ParseCNProviderRateLimitHeaders accepts common OpenAI-compatible spellings.
func ParseCNProviderRateLimitHeaders(headers http.Header) map[string]CNProviderRateLimitWindow {
	result := make(map[string]CNProviderRateLimitWindow)
	for _, dimension := range []string{"requests", "tokens"} {
		window := CNProviderRateLimitWindow{
			Limit:     parseHeaderInt64(headers, "x-ratelimit-limit-"+dimension, "x-rate-limit-limit-"+dimension),
			Remaining: parseHeaderInt64(headers, "x-ratelimit-remaining-"+dimension, "x-rate-limit-remaining-"+dimension),
			ResetAt:   parseHeaderTime(headers, "x-ratelimit-reset-"+dimension, "x-rate-limit-reset-"+dimension),
		}
		if window.Limit != nil || window.Remaining != nil || window.ResetAt != nil {
			result[dimension] = window
		}
	}
	return result
}

func parseHeaderInt64(headers http.Header, keys ...string) *int64 {
	for _, key := range keys {
		value := strings.TrimSpace(headers.Get(key))
		if value == "" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err == nil && n >= 0 {
			return &n
		}
	}
	return nil
}

func parseHeaderTime(headers http.Header, keys ...string) *time.Time {
	for _, key := range keys {
		value := strings.TrimSpace(headers.Get(key))
		if value == "" {
			continue
		}
		if duration, err := time.ParseDuration(value); err == nil {
			t := time.Now().Add(duration)
			return &t
		}
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			// Providers use both Unix seconds and a remaining-seconds delta.
			// Current Unix timestamps are large; small integers are treated as
			// durations so a header such as "60" cannot be mistaken for 1970.
			var t time.Time
			if seconds < time.Now().Unix()/2 {
				t = time.Now().Add(time.Duration(seconds) * time.Second)
			} else {
				t = time.Unix(seconds, 0)
			}
			return &t
		}
		if t, err := time.Parse(time.RFC3339, value); err == nil {
			return &t
		}
	}
	return nil
}
