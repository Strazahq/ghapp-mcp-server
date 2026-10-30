package main

import (
	"encoding/base64"
	"sync"
	"time"
)

// The two git operations, the op of a grant.
const (
	gitOpPush  = "push"
	gitOpFetch = "fetch"
)

var b64url = base64.RawURLEncoding.Strict()

// usedIDsMax bounds how many spent ids one instance remembers at once,
// about 100 bytes each.
const usedIDsMax = 1 << 16

// spentStore remembers the jti of each grant and proof that opened its git
// request, until it expires. usedIDs is the one in memory, per instance.
type spentStore interface {
	isUsed(key string) bool
	use(key string, until, now time.Time) bool
	full(now time.Time) bool
}

// usedIDs remembers each spent id until its grant or proof expires, so that
// each opens what it opens once only.
//
// It is per instance, in memory. With several ghapp instances behind a load
// balancer, each has its own, so a spent grant can be replayed once on each
// other instance within its 5 minutes, by the holder that has its key. A
// restart forgets it the same way.
type usedIDs struct {
	mu        sync.Mutex
	max       int
	until     map[string]time.Time // id to its expiry
	lastSweep time.Time
}

func newUsedIDs(max int) *usedIDs {
	return &usedIDs{max: max, until: map[string]time.Time{}}
}

// isUsed reports whether id was spent.
func (u *usedIDs) isUsed(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, used := u.until[id]
	return used
}

// use marks id spent until exp. It reports false when id was spent already,
// so of two requests with one id one wins.
func (u *usedIDs) use(id string, exp, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, used := u.until[id]; used {
		return false
	}
	u.sweep(now)
	u.until[id] = exp
	return true
}

// full reports whether the set holds max live ids even after the expired
// ones are dropped. The endpoint then takes no grant until some expire, so
// a grant is never let through unremembered. use itself never refuses, so
// requests already past this check can exceed max by as many as are in
// flight.
func (u *usedIDs) full(now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sweep(now)
	return len(u.until) >= u.max
}

// sweep drops the expired ids once the set is full, at most once a second,
// so a full set costs one pass a second and not one per request. The caller
// holds mu.
func (u *usedIDs) sweep(now time.Time) {
	if len(u.until) < u.max || now.Sub(u.lastSweep) < time.Second {
		return
	}
	u.lastSweep = now
	for id, exp := range u.until {
		if !now.Before(exp) {
			delete(u.until, id)
		}
	}
}
