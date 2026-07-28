package history

import (
	"sync"
	"time"
)

// defaultCacheCap bounds the cache so a bot watching many projects cannot grow
// without limit. At the cap the oldest entry is evicted.
const defaultCacheCap = 500

// cacheKey is a project plus the primary excluded ref: a baseline computed for
// one branch is not valid for another, because each excludes different samples.
type cacheKey struct {
	projectID int64
	ref       string
}

type cacheEntry struct {
	baseline  Baseline
	fetchedAt time.Time
}

// cache is a TTL map of reduced baselines. Nothing raw is kept — a hit costs no
// GitLab call and no recomputation.
type cache struct {
	mu  sync.Mutex
	ttl time.Duration
	cap int
	m   map[cacheKey]cacheEntry
}

func newCache(ttl time.Duration, capacity int) *cache {
	return &cache{ttl: ttl, cap: capacity, m: map[cacheKey]cacheEntry{}}
}

// get returns the cached baseline when it exists and is younger than the TTL,
// evicting it otherwise. A zero TTL disables caching entirely.
func (c *cache) get(k cacheKey, now time.Time) (Baseline, bool) {
	if c.ttl <= 0 {
		return Baseline{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return Baseline{}, false
	}
	if now.Sub(e.fetchedAt) >= c.ttl {
		delete(c.m, k)
		return Baseline{}, false
	}
	return e.baseline, true
}

func (c *cache) put(k cacheKey, b Baseline, now time.Time) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[k]; !exists && len(c.m) >= c.cap {
		c.evictOldestLocked()
	}
	c.m[k] = cacheEntry{baseline: b, fetchedAt: now}
}

// evictOldestLocked drops the least recently fetched entry. The caller holds mu.
func (c *cache) evictOldestLocked() {
	var (
		oldestKey cacheKey
		oldest    time.Time
		found     bool
	)
	for k, e := range c.m {
		if !found || e.fetchedAt.Before(oldest) {
			oldestKey, oldest, found = k, e.fetchedAt, true
		}
	}
	if found {
		delete(c.m, oldestKey)
	}
}
