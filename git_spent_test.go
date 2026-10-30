package main

import (
	"testing"
	"time"
)

// An id is remembered as spent until it expires, and a full set frees the
// expired ones, at most once a second.
func TestUsedIDs(t *testing.T) {
	u := newUsedIDs(2)
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	if u.isUsed("a") || !u.use("a", now.Add(100*time.Millisecond), now) || !u.isUsed("a") || u.use("a", now.Add(time.Minute), now) {
		t.Fatal("an id was not spent once and then refused")
	}
	if u.full(now) {
		t.Error("full with one of two")
	}
	u.use("b", now.Add(5*time.Minute), now)
	if !u.full(now) {
		t.Error("not full with two of two")
	}
	// "a" expired, but within a second of the last sweep nothing is
	// dropped. After that it is.
	if !u.full(now.Add(500 * time.Millisecond)) {
		t.Error("swept twice within a second")
	}
	later := now.Add(2 * time.Minute)
	if u.full(later) || u.isUsed("a") || !u.isUsed("b") {
		t.Errorf("after a's expiry: full %v, a used %v, b used %v", u.full(later), u.isUsed("a"), u.isUsed("b"))
	}
	// use never refuses a new id, even when full.
	u.use("c", later.Add(time.Minute), later)
	if !u.use("d", later.Add(time.Minute), later) || len(u.until) != 3 {
		t.Errorf("use refused a new id in a full set: %d held", len(u.until))
	}
}
