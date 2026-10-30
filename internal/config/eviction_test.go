package config

import (
	"math"
	"testing"
)

func TestCacheEvictionDefaultsAndValidation(t *testing.T) {
	got := CacheEviction{}.Normalized()
	if got.Admission.Policy != AdmitAlways || got.Admission.WindowHours != 168 || got.Admission.MinSize != 64<<20 {
		t.Fatalf("admission defaults = %+v", got.Admission)
	}
	if got.Strategy != EvictLRU || got.TargetPercent != 90 || got.Frequency.HalfLifeHours != 168 || got.Cost.Weight != 0.5 || got.Cost.Bandwidth != 50<<20 || got.Cost.LatencySeconds != 1 {
		t.Fatalf("defaults = %+v", got)
	}
	if err := (CacheEviction{}).Validate(); err != nil {
		t.Fatalf("the zero value must stay valid: %v", err)
	}
	for name, bad := range map[string]CacheEviction{
		"unknown strategy": {Strategy: "random"},
		"target":           {TargetPercent: 100},
		"negative target":  {TargetPercent: -1},
		"weight":           {Cost: CacheCost{Weight: 2.5}},
		"negative weight":  {Cost: CacheCost{Weight: -1}},
		"latency":          {Cost: CacheCost{LatencySeconds: -1}},
		"repository size":  {MaxRepositorySize: -1},
		"ring":             {FIFO: CacheFIFO{RingFiles: -1}},
		"half-life":        {Frequency: CacheFrequency{HalfLifeHours: maxDurationHours + 1}},
		"admission policy": {Admission: CacheAdmission{Policy: "sometimes"}},
		"admission window": {Admission: CacheAdmission{WindowHours: -1}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
}

func TestCacheEvictionFromTheEnvironment(t *testing.T) {
	t.Setenv("HFPROXY_CACHE_EVICTION_STRATEGY", "cost_lru")
	t.Setenv("HFPROXY_CACHE_COST_WEIGHT", "0.75")
	t.Setenv("HFPROXY_CACHE_MAX_REVISIONS", "3")
	t.Setenv("HFPROXY_CACHE_ADMISSION", "second_request")
	got, err := cacheEvictionFromEnvironment()
	if err != nil || got.Strategy != EvictCostLRU || got.Cost.Weight != 0.75 || got.MaxRevisions != 3 || got.Admission.Policy != AdmitSecondRequest {
		t.Fatalf("got %+v (%v)", got, err)
	}
	t.Setenv("HFPROXY_CACHE_COST_WEIGHT", "much")
	if _, err := cacheEvictionFromEnvironment(); err == nil {
		t.Fatal("a weight that is not a number was accepted")
	}
}

func TestSavedSettingsWithoutEvictionKeepTheEnvironment(t *testing.T) {
	base := Config{CacheEviction: CacheEviction{Strategy: EvictLFU}}
	if got := (managedSettings{}).apply(base).CacheEviction.Strategy; got != EvictLFU {
		t.Fatalf("got %q", got)
	}
	saved := managedFromConfig(Config{CacheEviction: CacheEviction{Strategy: EvictGDSF}})
	if got := saved.apply(base).CacheEviction.Strategy; got != EvictGDSF {
		t.Fatalf("got %q", got)
	}
}

func TestEvictionRejectsNaNAndInfiniteCosts(t *testing.T) {
	for name, e := range map[string]CacheEviction{
		"NaN weight":       {Cost: CacheCost{Weight: math.NaN()}},
		"NaN latency":      {Cost: CacheCost{LatencySeconds: math.NaN()}},
		"infinite latency": {Cost: CacheCost{LatencySeconds: math.Inf(1)}},
		"infinite weight":  {Cost: CacheCost{Weight: math.Inf(1)}},
	} {
		if err := e.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
