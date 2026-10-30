package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// A full cache forgets its least recently used entry first.
func TestLRUBoundAndOrder(t *testing.T) {
	c := newLRU[string, int](2)
	until := t0.Add(time.Hour)
	c.put("a", 1, until, t0)
	c.put("b", 2, until, t0)
	if _, ok := c.get("a", t0); !ok { // a is now the most recently used
		t.Fatal("a missing")
	}
	c.put("c", 3, until, t0)
	if _, ok := c.get("b", t0); ok {
		t.Error("b, the least recently used, was kept")
	}
	for k, want := range map[string]int{"a": 1, "c": 3} {
		if v, ok := c.get(k, t0); !ok || v != want {
			t.Errorf("get(%s) = %d, %v", k, v, ok)
		}
	}
	c.put("a", 9, until, t0)
	if v, _ := c.get("a", t0); v != 9 || c.len() != 2 {
		t.Errorf("a replaced = %d, len %d", v, c.len())
	}
	c.remove("a")
	if _, ok := c.get("a", t0); ok || c.len() != 1 {
		t.Errorf("a after remove: %v, len %d", ok, c.len())
	}
}

// An entry is good until its deadline, and expired entries are dropped when
// read and by the sweep once a minute.
func TestLRUExpiry(t *testing.T) {
	c := newLRU[string, int](10)
	c.put("short", 1, t0.Add(time.Second), t0)
	c.put("long", 2, t0.Add(time.Hour), t0)
	c.put("dead", 3, t0, t0) // a deadline that is not after now stores nothing
	if c.len() != 2 {
		t.Fatalf("len = %d, want 2", c.len())
	}
	if _, ok := c.get("short", t0.Add(999*time.Millisecond)); !ok {
		t.Error("short gone before its deadline")
	}
	if _, ok := c.get("short", t0.Add(time.Second)); ok || c.len() != 1 {
		t.Errorf("short at its deadline: %v, len %d", ok, c.len())
	}
	c.put("short2", 4, t0.Add(time.Second), t0)
	later := t0.Add(2 * sweepEvery)
	c.put("new", 5, later.Add(time.Hour), later) // sweeps
	if c.len() != 2 {
		t.Errorf("len after the sweep = %d, want long and new", c.len())
	}
}

// Concurrent calls for one key run fn once and share its result.
func TestFlightCollapses(t *testing.T) {
	var f flight[string, int]
	var runs atomic.Int32
	release := make(chan struct{})
	const n = 10
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := range n {
		wg.Go(func() {
			v, _, err := f.do(context.Background(), "k", func(context.Context) (int, error) {
				runs.Add(1)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		})
	}
	waitFor(t, func() bool { return runs.Load() == 1 && f.waiting("k") == n-1 })
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Errorf("fn ran %d times", runs.Load())
	}
	for i, v := range results {
		if v != 42 {
			t.Errorf("result %d = %d", i, v)
		}
	}
}

// A waiter whose own context ends stops waiting. A waiter that gets the
// runner's context error while its own is live runs fn itself.
func TestFlightContexts(t *testing.T) {
	var f flight[string, int]
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := f.do(leaderCtx, "k", func(ctx context.Context) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		})
		leaderDone <- err
	}()
	<-started

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, _, err := f.do(waiterCtx, "k", func(context.Context) (int, error) { return 1, nil })
		gaveUp <- err
	}()
	follower := make(chan int, 1)
	go func() {
		v, _, err := f.do(context.Background(), "k", func(context.Context) (int, error) { return 7, nil })
		if err != nil {
			t.Error(err)
		}
		follower <- v
	}()
	waitFor(t, func() bool { return f.waiting("k") == 2 })

	cancelWaiter()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Errorf("the waiter that gave up got %v", err)
	}
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader got %v", err)
	}
	if v := <-follower; v != 7 {
		t.Errorf("the follower got %d, want its own run's 7", v)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

var (
	agentA = Caller{Kind: callerAgent, Client: "straza-agent-build", Subject: "sa-build"}
	agentB = Caller{Kind: callerAgent, Client: "straza-agent-deploy", Subject: "sa-deploy"}
)

func testTokenCache(f *fakeGitHub, now *time.Time) *TokenCache {
	c := newTokenCache(f, newAuditLogger(io.Discard))
	if now != nil {
		c.now = func() time.Time { return *now }
		f.now = c.now
	}
	return c
}

// Two different callers never get the same token, for the same
// installation, repo and permissions. One caller gets its own token back.
func TestTokenCacheIsolatesCallers(t *testing.T) {
	f := newFakeGitHub()
	c := testTokenCache(f, nil)
	ctx := context.Background()
	callers := []Caller{
		agentA, agentB,
		// The quoting keeps these two apart.
		{Kind: callerAgent, Client: "x y", Subject: "z"},
		{Kind: callerAgent, Client: "x", Subject: "y z"},
		{Kind: callerGateway}, {Kind: callerListing}, {Kind: callerLocal},
	}
	seen := map[string]Caller{}
	for _, caller := range callers {
		tok, reused, err := c.TokenFor(ctx, caller, orgInst, []int64{payID}, permPullsWrite)
		if err != nil || reused {
			t.Fatalf("%+v: %v, reused %v", caller, err, reused)
		}
		if other, dup := seen[tok.Value]; dup {
			t.Errorf("%+v got the token of %+v", caller, other)
		}
		seen[tok.Value] = caller
	}
	for _, caller := range callers {
		tok, reused, err := c.TokenFor(ctx, caller, orgInst, []int64{payID}, permPullsWrite)
		if err != nil || !reused || seen[tok.Value] != caller {
			t.Errorf("%+v again: reused %v, got the token of %+v (%v)", caller, reused, seen[tok.Value], err)
		}
	}
	if mints, _ := f.snapshot(); len(mints) != len(callers) {
		t.Errorf("%d mints, want %d", len(mints), len(callers))
	}
}

// The key is the installation, the sorted repo ids and the permissions: a
// different set of any of them is another token, the same set in another
// order is the same one.
func TestTokenCacheKey(t *testing.T) {
	f := newFakeGitHub()
	c := testTokenCache(f, nil)
	ctx := context.Background()
	first, _, _ := c.TokenFor(ctx, agentA, orgInst, []int64{otherID, payID}, permMerge)
	same, reused, _ := c.TokenFor(ctx, agentA, orgInst, []int64{payID, otherID, payID}, github.Permissions{"pull_requests": "write", "contents": "write"})
	if !reused || same.Value != first.Value {
		t.Errorf("the same ids and permissions in another order minted again")
	}
	for _, tc := range []struct {
		inst  int64
		ids   []int64
		perms github.Permissions
	}{
		{orgInst, []int64{payID}, permMerge},
		{orgInst, nil, permMerge},
		{orgInst, []int64{otherID, payID}, permPullsWrite},
		{orgInst, []int64{otherID, payID}, github.Permissions{"contents": "read", "pull_requests": "write"}},
		{usrInst, []int64{otherID, payID}, permMerge},
	} {
		tok, reused, _ := c.TokenFor(ctx, agentA, tc.inst, tc.ids, tc.perms)
		if reused || tok.Value == first.Value {
			t.Errorf("%+v reused the token for another scope", tc)
		}
	}
	mints, _ := f.snapshot()
	if got := mints[0].RepoIDs; !reflect.DeepEqual(got, []int64{payID, otherID}) {
		t.Errorf("minted for %v, want the ids sorted", got)
	}
	if mints[2].RepoIDs != nil {
		t.Errorf("a token for the whole installation was minted with ids %v", mints[2].RepoIDs)
	}
}

// A token is reused while more than 5 minutes are left before it expires.
func TestTokenCacheExpiryMargin(t *testing.T) {
	f := newFakeGitHub()
	now := t0
	c := testTokenCache(f, &now)
	ctx := context.Background()
	first, _, _ := c.TokenFor(ctx, agentA, orgInst, []int64{payID}, permPullsRead)
	if !first.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the fake's token expires at %v", first.ExpiresAt)
	}
	now = t0.Add(55*time.Minute - time.Second)
	if tok, reused, _ := c.TokenFor(ctx, agentA, orgInst, []int64{payID}, permPullsRead); !reused || tok.Value != first.Value {
		t.Error("not reused with 5 minutes and a second left")
	}
	now = t0.Add(55 * time.Minute)
	second, reused, _ := c.TokenFor(ctx, agentA, orgInst, []int64{payID}, permPullsRead)
	if reused || second.Value == first.Value {
		t.Error("reused with 5 minutes left")
	}
	now = now.Add(time.Minute)
	if tok, reused, _ := c.TokenFor(ctx, agentA, orgInst, []int64{payID}, permPullsRead); !reused || tok.Value != second.Value {
		t.Error("the new token was not reused")
	}
	if mints, _ := f.snapshot(); len(mints) != 2 {
		t.Errorf("%d mints, want 2", len(mints))
	}
}

// Concurrent misses for one key mint once and share the token. Misses for
// another caller mint their own.
func TestTokenCacheConcurrentMissesMintOnce(t *testing.T) {
	f := newFakeGitHub()
	c := testTokenCache(f, nil)
	release := make(chan struct{})
	var entered atomic.Int32
	f.beforeMint = func() {
		entered.Add(1)
		<-release
	}
	const n = 20
	var wg sync.WaitGroup
	got := make([]github.Token, 2*n)
	reusedCount := atomic.Int32{}
	for i := range 2 * n {
		caller := agentA
		if i%2 == 1 {
			caller = agentB
		}
		wg.Go(func() {
			tok, reused, err := c.TokenFor(context.Background(), caller, orgInst, []int64{payID}, permPullsWrite)
			if err != nil {
				t.Error(err)
			}
			if reused {
				reusedCount.Add(1)
			}
			got[i] = tok
		})
	}
	keyA := newTokenKey(agentA, orgInst, []int64{payID}, permPullsWrite)
	keyB := newTokenKey(agentB, orgInst, []int64{payID}, permPullsWrite)
	waitFor(t, func() bool {
		return entered.Load() == 2 && c.flights.waiting(keyA) == n-1 && c.flights.waiting(keyB) == n-1
	})
	close(release)
	wg.Wait()

	if mints, _ := f.snapshot(); len(mints) != 2 {
		t.Fatalf("%d mints, want one per caller", len(mints))
	}
	for i := range got {
		if got[i].Value != got[i%2].Value {
			t.Errorf("call %d got another token than its caller's first", i)
		}
	}
	if got[0].Value == got[1].Value {
		t.Error("two callers share a token")
	}
	if reusedCount.Load() != 2*n-2 {
		t.Errorf("%d calls report reused, want %d", reusedCount.Load(), 2*n-2)
	}
}

// A token without a caller is refused, so a caller that forgot to say who
// it mints for never shares the local caller's tokens. A failed mint is
// never cached.
func TestTokenCacheRefusals(t *testing.T) {
	f := newFakeGitHub()
	c := testTokenCache(f, nil)
	ctx := context.Background()
	if _, _, err := c.TokenFor(ctx, Caller{}, orgInst, []int64{payID}, permPullsRead); !errors.Is(err, errNoCaller) {
		t.Errorf("no caller: %v", err)
	}
	f.errs["MintToken"] = &github.StatusError{StatusCode: 502, Status: "502 Bad Gateway"}
	if _, _, err := c.TokenFor(ctx, agentA, orgInst, []int64{payID}, permPullsRead); err == nil {
		t.Fatal("a failed mint gave a token")
	}
	delete(f.errs, "MintToken")
	if _, reused, err := c.TokenFor(ctx, agentA, orgInst, []int64{payID}, permPullsRead); err != nil || reused {
		t.Errorf("after a failed mint: %v, reused %v", err, reused)
	}
	if mints, _ := f.snapshot(); len(mints) != 2 {
		t.Errorf("%d mints, want the failed one and a new one", len(mints))
	}
}

// TokenHash is GitHub's hashed_token: standard base64 with padding of the
// token's SHA-256. The value is what
// echo -n ghs_16C7e42F292c6912E7710c838347Ae178B4a | openssl dgst -sha256 -binary | base64
// prints, for the example token on GitHub's REST page.
func TestTokenHashIsGitHubsForm(t *testing.T) {
	got := TokenHash(github.Token{Value: "ghs_16C7e42F292c6912E7710c838347Ae178B4a"})
	if want := "eXwM5BRYbMjraSmv6UfHIOPk01HdBwpv459jC+qrMFQ="; got != want {
		t.Errorf("TokenHash = %q, want %q", got, want)
	}
}
