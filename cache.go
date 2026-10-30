package main

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// sweepEvery is how often a put walks a whole cache to drop expired entries.
// Between sweeps an expired entry is dropped when it is read, or when it is
// the least recently used one and room is needed.
const sweepEvery = time.Minute

// lru is a map bounded to max entries, each with its own deadline. A full
// cache forgets its least recently used entry first. It is safe for
// concurrent use. Callers pass the time, so tests hold the clock.
type lru[K comparable, V any] struct {
	mu        sync.Mutex
	max       int
	order     *list.List // front is the most recently used
	items     map[K]*list.Element
	lastSweep time.Time
}

type lruEntry[K comparable, V any] struct {
	key   K
	val   V
	until time.Time // the entry is good while now is before until
}

func newLRU[K comparable, V any](max int) *lru[K, V] {
	return &lru[K, V]{max: max, order: list.New(), items: map[K]*list.Element{}}
}

// get returns the entry for k when it is still good at now, and marks it
// recently used. An entry past its deadline is dropped.
func (c *lru[K, V]) get(k K, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[k]
	if !ok {
		return zero, false
	}
	e := el.Value.(*lruEntry[K, V])
	if !now.Before(e.until) {
		c.order.Remove(el)
		delete(c.items, k)
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

// put stores v for k until the deadline, replacing any entry for k. A
// deadline that is not after now stores nothing. Once a minute put drops
// every expired entry, and it then drops least recently used entries until
// the cache holds at most max.
func (c *lru[K, V]) put(k K, v V, until, now time.Time) {
	if !now.Before(until) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.lastSweep) >= sweepEvery {
		c.sweep(now)
	}
	if el, ok := c.items[k]; ok {
		el.Value = &lruEntry[K, V]{key: k, val: v, until: until}
		c.order.MoveToFront(el)
		return
	}
	c.items[k] = c.order.PushFront(&lruEntry[K, V]{key: k, val: v, until: until})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*lruEntry[K, V]).key)
	}
}

// sweep drops every expired entry. The caller holds mu.
func (c *lru[K, V]) sweep(now time.Time) {
	c.lastSweep = now
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if e := el.Value.(*lruEntry[K, V]); !now.Before(e.until) {
			c.order.Remove(el)
			delete(c.items, e.key)
		}
		el = next
	}
}

// remove drops the entry for k, if any.
func (c *lru[K, V]) remove(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.order.Remove(el)
		delete(c.items, k)
	}
}

// len is the number of entries held, expired ones included until dropped.
func (c *lru[K, V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// flight collapses concurrent calls for one key into one: the first caller
// runs fn, and every caller that comes while it runs waits and shares its
// result. It keeps nothing once fn returns, so it never serves an old
// result: that is the cache's job.
type flight[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
}

type flightCall[V any] struct {
	done    chan struct{}
	waiters int // callers waiting on done, for tests
	val     V
	err     error
}

// do runs fn for key, or waits for the run already under way. shared reports
// that another caller ran fn. A waiter whose own context ends stops waiting
// with its context's error. A waiter that gets the runner's context error
// while its own context is still live runs again, so one caller that gives
// up does not fail the others.
func (f *flight[K, V]) do(ctx context.Context, key K, fn func(context.Context) (V, error)) (v V, shared bool, err error) {
	for {
		f.mu.Lock()
		if f.calls == nil {
			f.calls = map[K]*flightCall[V]{}
		}
		if c, ok := f.calls[key]; ok {
			c.waiters++
			f.mu.Unlock()
			select {
			case <-c.done:
			case <-ctx.Done():
				var zero V
				return zero, false, ctx.Err()
			}
			if isContextErr(c.err) && ctx.Err() == nil {
				continue
			}
			return c.val, true, c.err
		}
		c := &flightCall[V]{done: make(chan struct{})}
		f.calls[key] = c
		f.mu.Unlock()

		c.val, c.err = fn(ctx)
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
		return c.val, false, c.err
	}
}

// waiting is the number of callers waiting on the run for key, for tests.
func (f *flight[K, V]) waiting(key K) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.calls[key]; ok {
		return c.waiters
	}
	return 0
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
