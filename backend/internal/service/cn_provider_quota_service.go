package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	CNQuotaSnapshotExtraKey = "cn_quota_snapshot"
	cnQuotaSnapshotTTL      = 5 * time.Minute
	cnQuotaNegativeTTL      = time.Minute
	cnQuotaFetchTimeout     = 45 * time.Second
)

var quotaErrorURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

// CNProviderQuotaSnapshot is the provider-neutral quota/balance contract.
// Unknown/failed snapshots never imply zero remaining balance.
type CNProviderQuotaSnapshot struct {
	Source             string                               `json:"source,omitempty"`
	State              string                               `json:"state"` // fresh, stale, failed, unknown
	Remaining          *float64                             `json:"remaining,omitempty"`
	Limit              *float64                             `json:"limit,omitempty"`
	Unit               string                               `json:"unit,omitempty"`
	WindowStart        *time.Time                           `json:"window_start,omitempty"`
	ResetAt            *time.Time                           `json:"reset_at,omitempty"`
	FetchedAt          time.Time                            `json:"fetched_at"`
	ExpiresAt          *time.Time                           `json:"expires_at,omitempty"`
	NegativeCacheUntil *time.Time                           `json:"negative_cache_until,omitempty"`
	Error              string                               `json:"error,omitempty"`
	RateLimits         map[string]CNProviderRateLimitWindow `json:"rate_limits,omitempty"`
}

func (s *CNProviderQuotaSnapshot) normalize(now time.Time) {
	if s == nil {
		return
	}
	if s.State == "fresh" && s.ExpiresAt != nil && now.After(*s.ExpiresAt) {
		s.State = "stale"
	}
	if s.State == "" {
		s.State = "unknown"
	}
}

type CNProviderQuotaFetcher interface {
	FetchQuota(ctx context.Context, account *Account) (*CNProviderQuotaSnapshot, error)
}

type cnQuotaCacheEntry struct {
	snapshot *CNProviderQuotaSnapshot
	err      error
	at       time.Time
}

// CNProviderQuotaService owns cache, negative cache and persistence.
type CNProviderQuotaService struct {
	accountRepo AccountRepository
	fetcher     CNProviderQuotaFetcher
	mu          sync.Mutex
	cache       map[int64]cnQuotaCacheEntry
	inflight    map[int64]chan struct{}
}

func NewCNProviderQuotaService(accountRepo AccountRepository, fetcher CNProviderQuotaFetcher) *CNProviderQuotaService {
	return &CNProviderQuotaService{accountRepo: accountRepo, fetcher: fetcher, cache: make(map[int64]cnQuotaCacheEntry), inflight: make(map[int64]chan struct{})}
}

func (s *CNProviderQuotaService) GetSnapshot(ctx context.Context, account *Account, force bool) (*CNProviderQuotaSnapshot, error) {
	if account == nil || !isDomesticOpenAICompatiblePlatform(account.Platform) {
		return &CNProviderQuotaSnapshot{State: "unknown"}, nil
	}
	now := time.Now()
	s.mu.Lock()
	// Hydrate the in-memory cache from the durable account snapshot after a
	// process restart; only then decide whether a network refresh is needed.
	if !force {
		if raw, ok := account.Extra[CNQuotaSnapshotExtraKey]; ok {
			if hydrated := decodeCNQuotaSnapshot(raw); hydrated != nil {
				hydrated.normalize(now)
				if hydrated.State == "fresh" && hydrated.ExpiresAt != nil && now.Before(*hydrated.ExpiresAt) {
					s.cache[account.ID] = cnQuotaCacheEntry{snapshot: hydrated, at: hydrated.FetchedAt}
				}
			}
		}
	}
	if !force {
		if entry, ok := s.cache[account.ID]; ok {
			ttl := cnQuotaSnapshotTTL
			if entry.err != nil {
				ttl = cnQuotaNegativeTTL
			}
			if now.Sub(entry.at) >= ttl {
				delete(s.cache, account.ID)
			} else {
				snap := cloneCNQuotaSnapshot(entry.snapshot)
				err := entry.err
				s.mu.Unlock()
				if snap != nil {
					snap.normalize(now)
				}
				return snap, err
			}
		}
	}
	if ch, ok := s.inflight[account.ID]; ok {
		s.mu.Unlock()
		select {
		case <-ch:
			return s.GetSnapshot(ctx, account, false)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ch := make(chan struct{})
	s.inflight[account.ID] = ch
	s.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, cnQuotaFetchTimeout)
	var snap *CNProviderQuotaSnapshot
	var err error
	if s.fetcher == nil {
		err = errors.New("quota fetcher unavailable")
	} else {
		snap, err = s.fetcher.FetchQuota(fetchCtx, account)
	}
	cancel()
	if err != nil {
		until := now.Add(cnQuotaNegativeTTL)
		snap = &CNProviderQuotaSnapshot{Source: quotaSource(account), State: "failed", FetchedAt: now, NegativeCacheUntil: &until, Error: sanitizeQuotaError(err)}
	}
	if snap == nil {
		err = errors.New("quota provider returned empty snapshot")
		snap = &CNProviderQuotaSnapshot{Source: quotaSource(account), State: "failed", FetchedAt: now, Error: err.Error()}
	}
	snap.Source = firstNonEmptyQuota(snap.Source, quotaSource(account))
	snap.FetchedAt = now
	snap.normalize(now)
	if snap.State == "fresh" && snap.ExpiresAt == nil {
		expires := now.Add(cnQuotaSnapshotTTL)
		snap.ExpiresAt = &expires
	}
	var persistErr error
	if s.accountRepo != nil {
		if persistErr = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{CNQuotaSnapshotExtraKey: snap}); persistErr != nil {
			// The in-memory result remains usable for this request, but callers must
			// see the degraded durability state and retry/alert rather than assuming
			// restart persistence succeeded.
			persistErr = fmt.Errorf("persist quota snapshot: %w", persistErr)
			snap.State = "degraded"
			snap.Error = sanitizeQuotaError(persistErr)
		}
	}
	if persistErr != nil {
		err = persistErr
	}
	s.mu.Lock()
	s.cache[account.ID] = cnQuotaCacheEntry{snapshot: cloneCNQuotaSnapshot(snap), err: err, at: now}
	close(ch)
	delete(s.inflight, account.ID)
	s.mu.Unlock()
	return snap, err
}

func quotaSource(a *Account) string {
	if a == nil {
		return "unknown"
	}
	return a.Platform
}

func cloneCNQuotaSnapshot(in *CNProviderQuotaSnapshot) *CNProviderQuotaSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func sanitizeQuotaError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	msg = quotaErrorURLPattern.ReplaceAllStringFunc(msg, func(raw string) string {
		trailing := ""
		for len(raw) > 0 && strings.ContainsRune(".,;:)]}", rune(raw[len(raw)-1])) {
			trailing = string(raw[len(raw)-1]) + trailing
			raw = raw[:len(raw)-1]
		}
		return sanitizeQuotaSource(raw) + trailing
	})
	if len(msg) > 256 {
		msg = msg[:256]
	}
	return msg
}

func firstNonEmptyQuota(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// HTTPCNProviderQuotaFetcher supports provider-specific quota_url/balance_url
// configuration and a small common JSON shape. It never logs credentials/body.
type HTTPCNProviderQuotaFetcher struct {
	httpUpstream HTTPUpstream
	cfg          *config.Config
	tlsProfiles  *TLSFingerprintProfileService
}

func NewHTTPCNProviderQuotaFetcher(httpUpstream HTTPUpstream, cfg *config.Config, tlsProfiles *TLSFingerprintProfileService) *HTTPCNProviderQuotaFetcher {
	return &HTTPCNProviderQuotaFetcher{httpUpstream: httpUpstream, cfg: cfg, tlsProfiles: tlsProfiles}
}

func (f *HTTPCNProviderQuotaFetcher) FetchQuota(ctx context.Context, account *Account) (*CNProviderQuotaSnapshot, error) {
	if f == nil || f.httpUpstream == nil || account == nil {
		return nil, errors.New("quota fetcher unavailable")
	}
	quotaURL := firstNonEmptyQuota(account.GetCredential("quota_url"), account.GetCredential("balance_url"))
	if quotaURL == "" {
		return nil, errors.New("quota_url is not configured")
	}
	url, err := f.validateQuotaURL(quotaURL)
	if err != nil {
		return nil, fmt.Errorf("invalid quota_url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build quota request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key := account.GetOpenAIApiKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	profile := (*tlsfingerprint.Profile)(nil)
	if f.tlsProfiles != nil {
		profile = f.tlsProfiles.ResolveTLSProfile(account)
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := f.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, profile)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota endpoint returned %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid quota response")
	}
	now := time.Now()
	snap := &CNProviderQuotaSnapshot{Source: sanitizeQuotaSource(url), State: "fresh", Unit: "requests", FetchedAt: now}
	snap.Remaining = numberPointer(raw, "remaining", "remaining_balance", "balance", "credits")
	snap.Limit = numberPointer(raw, "limit", "quota", "total")
	snap.ResetAt = timePointer(raw, "reset_at", "resetAt", "resets_at")
	snap.RateLimits = ParseCNProviderRateLimitHeaders(resp.Header)
	if snap.Remaining == nil && snap.Limit == nil && snap.ResetAt == nil {
		return nil, errors.New("quota response has no recognized fields")
	}
	if snap.ResetAt != nil {
		snap.ExpiresAt = snap.ResetAt
	}
	return snap, nil
}

func (f *HTTPCNProviderQuotaFetcher) validateQuotaURL(raw string) (string, error) {
	if f.cfg == nil {
		return "", errors.New("config is not available")
	}
	if !f.cfg.Security.URLAllowlist.Enabled {
		return urlvalidator.ValidateURLFormat(raw, f.cfg.Security.URLAllowlist.AllowInsecureHTTP)
	}
	return urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
		AllowedHosts:     f.cfg.Security.URLAllowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     f.cfg.Security.URLAllowlist.AllowPrivateHosts,
	})
}

func decodeCNQuotaSnapshot(raw any) *CNProviderQuotaSnapshot {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snapshot CNProviderQuotaSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

func sanitizeQuotaSource(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "configured"
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.EscapedPath(), "/")
}

func numberPointer(raw map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return &n
		case json.Number:
			x, _ := n.Float64()
			return &x
		case string:
			x, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err == nil {
				return &x
			}
		}
	}
	return nil
}

func timePointer(raw map[string]any, keys ...string) *time.Time {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		switch value := v.(type) {
		case string:
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				return &t
			}
		case float64:
			t := time.Unix(int64(value), 0)
			return &t
		}
	}
	return nil
}
