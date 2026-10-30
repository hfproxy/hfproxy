package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

// The eviction strategies. docs/cache-strategies.md compares them.
const (
	EvictLRU     = "lru"
	EvictFIFO    = "fifo"
	EvictLFU     = "lfu"
	EvictCostLRU = "cost_lru"
	EvictGDSF    = "gdsf"
)

// The admission policies: whether a file that is not in the cache is stored
// when a client asks for it, or streamed from the Hub without being kept.
const (
	AdmitAlways        = "always"
	AdmitSecondRequest = "second_request"
	AdmitFrequency     = "frequency"
)

const (
	defaultAdmissionWindowHours  = 7 * 24
	defaultAdmissionMinSize      = 64 << 20
	defaultEvictionTargetPercent = 90
	defaultHalfLifeHours         = 7 * 24
	defaultCostWeight            = 0.5
	defaultCostBandwidth         = 50 << 20
	defaultCostLatencySeconds    = 1
	maxCostWeight                = 2
	// maxDurationHours keeps an hour count inside a time.Duration.
	maxDurationHours = int64(1<<63-1) / int64(time.Hour)
)

// CacheEviction chooses which files leave the cache, and when. A limit left at
// 0 is off and the strategy settings fall back to their defaults, so the zero
// value behaves like the plain LRU of the previous releases.
type CacheEviction struct {
	Strategy string `json:"strategy"`
	// TargetPercent is where a size limit that was exceeded brings usage back
	// to, as a percentage of that limit.
	TargetPercent int64 `json:"target_percent"`
	// These limits apply whatever the strategy, on top of CacheMaxTotalSize.
	MaxRepositorySize int64 `json:"max_repository_size"`
	MaxNamespaceSize  int64 `json:"max_namespace_size"`
	MaxRevisions      int64 `json:"max_revisions"`
	MaxIdleHours      int64 `json:"max_idle_hours"`
	// What only some strategies read. All are kept when the strategy changes.
	FIFO      CacheFIFO      `json:"fifo"`
	Frequency CacheFrequency `json:"frequency"`
	Cost      CacheCost      `json:"cost"`
	// Admission decides whether a miss is stored at all.
	Admission CacheAdmission `json:"admission"`
}

// CacheAdmission filters what enters the cache. A file smaller than MinSize is
// always stored. second_request stores a file only when it was already asked
// for within WindowHours; frequency stores it only when the cache has room,
// or when it was asked for more often in the window than the file that would
// be evicted to make room has been used.
type CacheAdmission struct {
	Policy      string `json:"policy"`
	WindowHours int64  `json:"window_hours"`
	MinSize     int64  `json:"min_size"`
}

// Window is how long a request counts for admission.
func (a CacheAdmission) Window() time.Duration {
	if a.WindowHours == 0 {
		return defaultAdmissionWindowHours * time.Hour
	}
	return time.Duration(a.WindowHours) * time.Hour
}

// CacheFIFO sizes the circular cache of the fifo strategy, in bytes and in
// files; the first bound reached applies, and CacheMaxTotalSize still holds.
type CacheFIFO struct {
	RingSize  int64 `json:"ring_size"`
	RingFiles int64 `json:"ring_files"`
}

// CacheFrequency is read by lfu and gdsf: a use counts half as much after
// HalfLifeHours.
type CacheFrequency struct {
	HalfLifeHours int64 `json:"half_life_hours"`
}

// CacheCost is read by cost_lru and gdsf. Bandwidth and LatencySeconds estimate
// the cost of a file whose fill was not measured; Weight is how much cost_lru
// lets cost outweigh idle time (1 = a file twice as costly may idle twice as
// long).
type CacheCost struct {
	Weight         float64 `json:"weight"`
	Bandwidth      int64   `json:"bandwidth"`
	LatencySeconds float64 `json:"latency_seconds"`
}

// Normalized returns the settings with the defaults filled in.
func (e CacheEviction) Normalized() CacheEviction {
	if e.Strategy == "" {
		e.Strategy = EvictLRU
	}
	if e.TargetPercent == 0 {
		e.TargetPercent = defaultEvictionTargetPercent
	}
	if e.Frequency.HalfLifeHours == 0 {
		e.Frequency.HalfLifeHours = defaultHalfLifeHours
	}
	if e.Cost.Weight == 0 {
		e.Cost.Weight = defaultCostWeight
	}
	if e.Cost.Bandwidth == 0 {
		e.Cost.Bandwidth = defaultCostBandwidth
	}
	if e.Cost.LatencySeconds == 0 {
		e.Cost.LatencySeconds = defaultCostLatencySeconds
	}
	if e.Admission.Policy == "" {
		e.Admission.Policy = AdmitAlways
	}
	if e.Admission.WindowHours == 0 {
		e.Admission.WindowHours = defaultAdmissionWindowHours
	}
	if e.Admission.MinSize == 0 {
		e.Admission.MinSize = defaultAdmissionMinSize
	}
	return e
}

// HalfLife is the time after which a use counts half as much.
func (e CacheEviction) HalfLife() time.Duration {
	return time.Duration(e.Normalized().Frequency.HalfLifeHours) * time.Hour
}

// Validate rejects what the strategies cannot work with.
func (e CacheEviction) Validate() error {
	switch e.Strategy {
	case "", EvictLRU, EvictFIFO, EvictLFU, EvictCostLRU, EvictGDSF:
	default:
		return fmt.Errorf("the cache strategy must be one of %s, %s, %s, %s or %s", EvictLRU, EvictFIFO, EvictLFU, EvictCostLRU, EvictGDSF)
	}
	switch e.Admission.Policy {
	case "", AdmitAlways, AdmitSecondRequest, AdmitFrequency:
	default:
		return fmt.Errorf("the cache admission policy must be one of %s, %s or %s", AdmitAlways, AdmitSecondRequest, AdmitFrequency)
	}
	if e.Admission.WindowHours < 0 || e.Admission.WindowHours > maxDurationHours || e.Admission.MinSize < 0 {
		return fmt.Errorf("the cache admission window and minimum size must not be negative")
	}
	if e.TargetPercent < 0 || e.TargetPercent > 99 {
		return fmt.Errorf("the eviction target must be between 1 and 99 percent of the limit (0 means %d)", defaultEvictionTargetPercent)
	}
	if math.IsNaN(e.Cost.Weight) || math.IsNaN(e.Cost.LatencySeconds) || math.IsInf(e.Cost.LatencySeconds, 0) {
		return fmt.Errorf("the cost weight and the estimated fetch latency must be numbers")
	}
	if e.Cost.Weight < 0 || e.Cost.Weight > maxCostWeight {
		return fmt.Errorf("the cost weight must be above 0 and at most %d (0 means %g)", maxCostWeight, defaultCostWeight)
	}
	for _, limit := range []int64{e.MaxRepositorySize, e.MaxNamespaceSize, e.MaxRevisions, e.MaxIdleHours, e.FIFO.RingSize, e.FIFO.RingFiles, e.Frequency.HalfLifeHours, e.Cost.Bandwidth} {
		if limit < 0 {
			return fmt.Errorf("the cache eviction limits must not be negative")
		}
	}
	if e.MaxIdleHours > maxDurationHours || e.Frequency.HalfLifeHours > maxDurationHours {
		return fmt.Errorf("the cache eviction durations must be at most %d hours", maxDurationHours)
	}
	if e.Cost.LatencySeconds < 0 {
		return fmt.Errorf("the estimated fetch latency must not be negative")
	}
	return nil
}

func cacheEvictionFromEnvironment() (CacheEviction, error) {
	weight, err := decimal("CACHE_COST_WEIGHT")
	if err != nil {
		return CacheEviction{}, err
	}
	latency, err := decimal("CACHE_COST_LATENCY_SECONDS")
	if err != nil {
		return CacheEviction{}, err
	}
	return CacheEviction{
		Strategy:          value("CACHE_EVICTION_STRATEGY", ""),
		TargetPercent:     int64(number("CACHE_EVICTION_TARGET_PERCENT", 0)),
		MaxRepositorySize: int64(number("CACHE_MAX_REPOSITORY_SIZE", 0)),
		MaxNamespaceSize:  int64(number("CACHE_MAX_NAMESPACE_SIZE", 0)),
		MaxRevisions:      int64(number("CACHE_MAX_REVISIONS", 0)),
		MaxIdleHours:      int64(number("CACHE_MAX_IDLE_HOURS", 0)),
		FIFO:              CacheFIFO{RingSize: int64(number("CACHE_FIFO_RING_SIZE", 0)), RingFiles: int64(number("CACHE_FIFO_RING_FILES", 0))},
		Frequency:         CacheFrequency{HalfLifeHours: int64(number("CACHE_FREQUENCY_HALF_LIFE_HOURS", 0))},
		Cost:              CacheCost{Weight: weight, Bandwidth: int64(number("CACHE_COST_BANDWIDTH", 0)), LatencySeconds: latency},
		Admission:         CacheAdmission{Policy: value("CACHE_ADMISSION", ""), WindowHours: int64(number("CACHE_ADMISSION_WINDOW_HOURS", 0)), MinSize: int64(number("CACHE_ADMISSION_MIN_SIZE", 0))},
	}, nil
}

func decimal(key string) (float64, error) {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	return v, nil
}
