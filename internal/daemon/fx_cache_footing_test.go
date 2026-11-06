package daemon

import (
	"testing"
	"time"
)

func TestPortfolioFootingRateComesFromTheStoreWithinItsAge(t *testing.T) {
	now := time.Date(2026, 11, 6, 21, 0, 0, 0, time.UTC)
	cache := newFXRateCache()
	cache.now = func() time.Time { return now }
	cache.put("EUR", "USD", 0.8)
	s := &Server{fxRates: cache}

	if rate, ok := s.portfolioFootingFXRate("EUR", "USD"); !ok || rate != 0.8 {
		t.Fatalf("stored rate = %v, %v", rate, ok)
	}
	if _, ok := s.portfolioFootingFXRate("EUR", "CHF"); ok {
		t.Fatal("a currency with no stored rate must read as unknown")
	}
	now = now.Add(fxCacheTTL + time.Minute)
	if _, ok := s.portfolioFootingFXRate("EUR", "USD"); ok {
		t.Fatal("a rate past its age must read as unknown")
	}
}
