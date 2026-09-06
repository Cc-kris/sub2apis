package repository

import (
	"testing"
	"time"
)

func TestLocalDayStartUsesTargetTimezoneMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	got := localDayStart(time.Date(2026, 8, 25, 3, 30, 0, 0, time.UTC), loc)
	want := time.Date(2026, 8, 25, 0, 0, 0, 0, loc)
	if !got.Equal(want) || got.Location() != loc {
		t.Fatalf("localDayStart() = %s (%s), want %s (%s)", got, got.Location(), want, want.Location())
	}
}
