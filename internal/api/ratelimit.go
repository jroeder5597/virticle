package api

import (
	"sync"
	"time"
)

// limiter slows down password guessing. Each key counts failures; once a key
// reaches its threshold it is locked, and every further failure doubles the
// lock up to a cap. Failures older than the window are forgotten. State is in
// memory and starts empty after a restart.
type limiter struct {
	mu  sync.Mutex
	m   map[string]*limEntry
	now func() time.Time
}

type limEntry struct {
	fails int
	last  time.Time
	until time.Time
}

const (
	limWindow   = 15 * time.Minute
	limBaseLock = 30 * time.Second
	limMaxLock  = 15 * time.Minute
)

func newLimiter() *limiter { return &limiter{m: map[string]*limEntry{}, now: time.Now} }

// locked returns how long the longest lock among keys still has to run, or 0.
func (l *limiter) locked(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	now := l.now()
	for _, k := range keys {
		if e := l.m[k]; e != nil && e.until.After(now) && e.until.Sub(now) > wait {
			wait = e.until.Sub(now)
		}
	}
	return wait
}

// fail records a failure for key, which locks once threshold failures have
// happened within the window.
func (l *limiter) fail(key string, threshold int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) > 2000 {
		for k, e := range l.m {
			if now.Sub(e.last) > limWindow && !e.until.After(now) {
				delete(l.m, k)
			}
		}
	}
	e := l.m[key]
	if e == nil {
		e = &limEntry{}
		l.m[key] = e
	}
	if now.Sub(e.last) > limWindow && !e.until.After(now) {
		e.fails = 0
	}
	e.fails++
	e.last = now
	if e.fails >= threshold {
		shift := e.fails - threshold
		if shift > 6 {
			shift = 6
		}
		d := limBaseLock << shift
		if d > limMaxLock {
			d = limMaxLock
		}
		e.until = now.Add(d)
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}
