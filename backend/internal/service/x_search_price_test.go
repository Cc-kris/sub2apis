package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type xSearchPriceRepoStub struct{ value string }

func (r *xSearchPriceRepoStub) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (r *xSearchPriceRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if key == SettingKeyXSearchPricePerRequest && r.value != "" {
		return r.value, nil
	}
	return "", ErrSettingNotFound
}
func (r *xSearchPriceRepoStub) Set(_ context.Context, key, value string) error {
	if key == SettingKeyXSearchPricePerRequest {
		r.value = value
	}
	return nil
}
func (r *xSearchPriceRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *xSearchPriceRepoStub) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *xSearchPriceRepoStub) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *xSearchPriceRepoStub) Delete(context.Context, string) error { return nil }

func TestXSearchPriceContract(t *testing.T) {
	repo := &xSearchPriceRepoStub{}
	svc := NewSettingService(repo, &config.Config{})
	if _, err := NormalizeXSearchPrice("0"); err == nil {
		t.Fatal("zero price must be rejected")
	}
	if _, err := NormalizeXSearchPrice("1.12345678901"); err == nil {
		t.Fatal("more than 10 fractional digits must be rejected")
	}
	got, err := svc.SetXSearchPricePerRequest(context.Background(), "0.1250000000")
	if err != nil || got != "0.1250000000" {
		t.Fatalf("set price: got=%q err=%v", got, err)
	}
	if got, err := svc.GetXSearchPricePerRequest(context.Background()); err != nil || got != "0.1250000000" {
		t.Fatalf("get price: got=%q err=%v", got, err)
	}
	if got, err := svc.SetXSearchPricePerRequest(context.Background(), ""); err != nil || got != "" {
		t.Fatalf("clear price: got=%q err=%v", got, err)
	}
}
