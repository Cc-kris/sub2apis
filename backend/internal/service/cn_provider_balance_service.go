package service

import (
	"context"
	"errors"
)

// CNProviderBalanceService exposes a balance-oriented view over the shared
// quota resolver. It keeps balance and usage derivation separate while using
// one cache, one negative-cache policy and one persistence contract.
type CNProviderBalanceService struct {
	quota *CNProviderQuotaService
}

func NewCNProviderBalanceService(quota *CNProviderQuotaService) *CNProviderBalanceService {
	return &CNProviderBalanceService{quota: quota}
}

func (s *CNProviderBalanceService) GetBalance(ctx context.Context, account *Account, force bool) (*float64, *CNProviderQuotaSnapshot, error) {
	if s == nil || s.quota == nil {
		return nil, nil, errors.New("balance resolver unavailable")
	}
	snapshot, err := s.quota.GetSnapshot(ctx, account, force)
	if snapshot == nil {
		return nil, nil, err
	}
	if snapshot.Remaining == nil {
		return nil, snapshot, err
	}
	value := *snapshot.Remaining
	return &value, snapshot, err
}
