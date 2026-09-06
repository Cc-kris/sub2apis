//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestSelectHistoricalUpstreamPriceOrder(t *testing.T) {
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	fast := "fast"
	versions := []*dbent.UpstreamModelPriceVersion{
		{ID: 1, ModelPattern: "gpt-*", IsWildcard: true, EffectiveFrom: now.Add(-time.Hour)},
		{ID: 2, ModelPattern: "gpt-5*", IsWildcard: true, EffectiveFrom: now.Add(-2 * time.Hour)},
		{ID: 3, ModelPattern: "gpt-5.5", EffectiveFrom: now.Add(-3 * time.Hour)},
		{ID: 4, ModelPattern: "gpt-5.5", ServiceTier: &fast, EffectiveFrom: now.Add(-4 * time.Hour)},
	}

	require.Equal(t, int64(4), selectHistoricalUpstreamPrice(versions, "GPT-5.5", "fast").ID)
	require.Equal(t, int64(3), selectHistoricalUpstreamPrice(versions, "GPT-5.5", "standard").ID)
	require.Equal(t, int64(2), selectHistoricalUpstreamPrice(versions, "gpt-5.4", "standard").ID)
	require.Nil(t, selectHistoricalUpstreamPrice(versions, "claude-test", "standard"))
}

func TestFinanceExchangeRate(t *testing.T) {
	require.Equal(t, "1", financeExchangeRate("USD").String())
	require.Equal(t, "0.14", financeExchangeRate("CNY", map[string]any{"usd_exchange_rate": "0.14"}).String())
	require.True(t, financeExchangeRate("CNY", map[string]any{"usd_exchange_rate": "bad"}).IsZero())
}

func TestEnsureFXRateVersionReusesSameCurrencyAndEffectiveFrom(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })

	effective := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	created, err := client.FinanceFXRateVersion.Create().
		SetCurrency("USD").SetRateToUsd(decimal.NewFromInt(1)).SetSource("finance_initialization").
		SetObservedAt(effective).SetEffectiveFrom(effective).SetChecksum("existing").Save(context.Background())
	require.NoError(t, err)

	repo := &financePriceLookupRepository{client: client}
	got, err := repo.ensureFXRateVersion(context.Background(), "USD", decimal.NewFromInt(1), "currency_identity", effective, effective)
	require.NoError(t, err)
	require.Equal(t, created.ID, *got)
	count, err := client.FinanceFXRateVersion.Query().Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count)
}
