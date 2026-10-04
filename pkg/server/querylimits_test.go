package server

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeClock is an injected clock: every After call registers a timer that the
// test fires by hand, so no test sleeps or races a real timer.
type fakeClock struct {
	mu      sync.Mutex
	waits   []time.Duration
	timers  []chan time.Time
	created chan struct{}
}

func newFakeClock() *fakeClock { return &fakeClock{created: make(chan struct{}, 16)} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, d)
	c.timers = append(c.timers, ch)
	c.created <- struct{}{}
	return ch
}

func (c *fakeClock) fire(i int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers[i] <- time.Time{}
}

type acquireResult struct {
	release func()
	ok      bool
}

func acquireAsync(ctx context.Context, g *queryGate, route queryRoute) <-chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		release, ok := g.acquire(ctx, route)
		out <- acquireResult{release, ok}
	}()
	return out
}

func TestDefaultQueryLimits(t *testing.T) {
	got := DefaultQueryLimits()
	want := QueryLimits{
		Timeout:        10 * time.Second,
		InMemory:       2,
		Database:       4,
		QueueWait:      time.Second,
		MaxSourceRows:  100_000,
		MaxSourceBytes: 64 << 20,
		JoinEngines:    []string{"sqlite", "ingitdb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultQueryLimits() = %+v, want %+v", got, want)
	}
	got.JoinEngines[0] = "mutated"
	if DefaultQueryLimits().JoinEngines[0] != "sqlite" {
		t.Fatal("DefaultQueryLimits shares its JoinEngines slice between calls")
	}
}

func TestWithQueryLimitsStoresLimits(t *testing.T) {
	s := New("test", nil, WithQueryLimits(QueryLimits{Timeout: 3 * time.Second, InMemory: 1, JoinEngines: []string{"sqlite"}}))
	got := s.queryLimits
	if got.Timeout != 3*time.Second || got.InMemory != 1 || !reflect.DeepEqual(got.JoinEngines, []string{"sqlite"}) {
		t.Fatalf("explicit fields not kept: %+v", got)
	}
	def := DefaultQueryLimits()
	if got.Database != def.Database || got.QueueWait != def.QueueWait ||
		got.MaxSourceRows != def.MaxSourceRows || got.MaxSourceBytes != def.MaxSourceBytes {
		t.Fatalf("unset fields did not take defaults: %+v", got)
	}
	if s.queryGate == nil {
		t.Fatal("WithQueryLimits left the gate unset")
	}
}

func TestNewWithoutQueryLimitsUsesDefaults(t *testing.T) {
	s := New("test", nil)
	if !reflect.DeepEqual(s.queryLimits, DefaultQueryLimits()) {
		t.Fatalf("queryLimits = %+v, want defaults", s.queryLimits)
	}
	release, ok := s.queryGate.acquire(context.Background(), routeInMemory)
	if !ok {
		t.Fatal("default gate refused the first query")
	}
	release()
}

func TestNormalizeQueryLimits(t *testing.T) {
	got := normalizeQueryLimits(QueryLimits{InMemory: -1, Database: -5, Timeout: -1, MaxSourceRows: -1, MaxSourceBytes: -1, QueueWait: -1})
	def := DefaultQueryLimits()
	if got.QueueWait >= 0 {
		t.Errorf("negative QueueWait must stay negative (no queueing), got %v", got.QueueWait)
	}
	got.QueueWait = def.QueueWait
	if !reflect.DeepEqual(got, def) {
		t.Fatalf("normalize = %+v, want %+v", got, def)
	}
}

func TestGateRefusesThirdInMemoryQueryAfterWaitAndAdmitsAfterRelease(t *testing.T) {
	clock := newFakeClock()
	g := newQueryGate(QueryLimits{InMemory: 2, Database: 4, QueueWait: time.Second}, clock)
	ctx := context.Background()

	r1, ok1 := g.acquire(ctx, routeInMemory)
	r2, ok2 := g.acquire(ctx, routeInMemory)
	if !ok1 || !ok2 {
		t.Fatalf("first two in-memory queries must be admitted: %v %v", ok1, ok2)
	}

	third := acquireAsync(ctx, g, routeInMemory)
	<-clock.created
	if clock.waits[0] != time.Second {
		t.Fatalf("queue wait = %v, want 1s", clock.waits[0])
	}
	clock.fire(0)
	if res := <-third; res.ok || res.release != nil {
		t.Fatalf("third query must be refused after the wait, got ok=%v", res.ok)
	}

	r1()
	again, ok := g.acquire(ctx, routeInMemory)
	if !ok {
		t.Fatal("a query must be admitted after a release")
	}
	again()
	r2()
}

func TestGateAdmitsQueuedQueryWhenSlotFreesWithinWait(t *testing.T) {
	clock := newFakeClock()
	g := newQueryGate(QueryLimits{InMemory: 1, Database: 1, QueueWait: time.Second}, clock)
	ctx := context.Background()
	r1, _ := g.acquire(ctx, routeInMemory)

	waiter := acquireAsync(ctx, g, routeInMemory)
	<-clock.created
	r1()
	res := <-waiter
	if !res.ok {
		t.Fatal("queued query must be admitted when a slot frees inside the wait")
	}
	res.release()
}

func TestGateReleaseIsIdempotent(t *testing.T) {
	g := newQueryGate(QueryLimits{InMemory: 1, Database: 1, QueueWait: -1}, newFakeClock())
	ctx := context.Background()
	release, _ := g.acquire(ctx, routeInMemory)
	release()
	release() // must not free a slot it no longer holds
	a, okA := g.acquire(ctx, routeInMemory)
	_, okB := g.acquire(ctx, routeInMemory)
	if !okA || okB {
		t.Fatalf("double release leaked a slot: first=%v second=%v", okA, okB)
	}
	a()
}

func TestGateRoutesAreIndependent(t *testing.T) {
	g := newQueryGate(QueryLimits{InMemory: 1, Database: 2, QueueWait: -1}, newFakeClock())
	ctx := context.Background()
	if _, ok := g.acquire(ctx, routeInMemory); !ok {
		t.Fatal("in-memory slot refused")
	}
	if _, ok := g.acquire(ctx, routeInMemory); ok {
		t.Fatal("second in-memory query admitted past the limit of 1")
	}
	for i := 0; i < 2; i++ {
		if _, ok := g.acquire(ctx, routeDatabase); !ok {
			t.Fatalf("database query %d refused while in-memory is full", i)
		}
	}
	if _, ok := g.acquire(ctx, routeDatabase); ok {
		t.Fatal("third database query admitted past the limit of 2")
	}
}

func TestGateUnknownRouteIsRefused(t *testing.T) {
	g := newQueryGate(DefaultQueryLimits(), newFakeClock())
	if release, ok := g.acquire(context.Background(), queryRoute("mystery")); ok || release != nil {
		t.Fatalf("unknown route must be refused, got ok=%v", ok)
	}
}

func TestGateCancelledContextIsRefused(t *testing.T) {
	clock := newFakeClock()
	g := newQueryGate(QueryLimits{InMemory: 1, Database: 1, QueueWait: time.Hour}, clock)
	r1, _ := g.acquire(context.Background(), routeInMemory)
	defer r1()

	ctx, cancel := context.WithCancel(context.Background())
	waiter := acquireAsync(ctx, g, routeInMemory)
	<-clock.created
	cancel()
	if res := <-waiter; res.ok {
		t.Fatal("cancelled context must not be admitted")
	}
}

func TestGateCancelledContextRefusedWhenFull(t *testing.T) {
	g := newQueryGate(QueryLimits{InMemory: 1, Database: 1, QueueWait: -1}, newFakeClock())
	r1, _ := g.acquire(context.Background(), routeInMemory)
	defer r1()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := g.acquire(ctx, routeInMemory); ok {
		t.Fatal("full gate with a cancelled context must refuse")
	}
}

func TestRealClockAfter(t *testing.T) {
	select {
	case <-realClock{}.After(0):
	case <-time.After(5 * time.Second):
		t.Fatal("realClock.After(0) never fired")
	}
}

func TestRelationalCacheControl(t *testing.T) {
	ok := func(ttl time.Duration) cacheFacts { return cacheFacts{TTL: ttl} }
	tests := []struct {
		name     string
		readOnly bool
		auth     bool
		method   string
		dbs      []cacheFacts
		want     string
	}{
		{"single database", true, false, "GET", []cacheFacts{ok(time.Hour)}, "public, max-age=3600, s-maxage=3600"},
		{"minimum of several", true, false, "GET", []cacheFacts{ok(time.Hour), ok(30 * time.Second), ok(10 * time.Minute)}, "public, max-age=30, s-maxage=30"},
		{"one second", true, false, "GET", []cacheFacts{ok(time.Second)}, "public, max-age=1, s-maxage=1"},
		{"max ttl", true, false, "GET", []cacheFacts{ok(365 * 24 * time.Hour)}, "public, max-age=31536000, s-maxage=31536000"},
		{"not read only", false, false, "GET", []cacheFacts{ok(time.Hour)}, "no-store"},
		{"auth enabled", true, true, "GET", []cacheFacts{ok(time.Hour)}, "no-store"},
		{"POST", true, false, "POST", []cacheFacts{ok(time.Hour)}, "no-store"},
		{"HEAD", true, false, "HEAD", []cacheFacts{ok(time.Hour)}, "no-store"},
		{"lowercase get", true, false, "get", []cacheFacts{ok(time.Hour)}, "no-store"},
		{"empty method", true, false, "", []cacheFacts{ok(time.Hour)}, "no-store"},
		{"no databases", true, false, "GET", nil, "no-store"},
		{"empty databases", true, false, "GET", []cacheFacts{}, "no-store"},
		{"zero ttl", true, false, "GET", []cacheFacts{ok(0)}, "no-store"},
		{"one zero ttl among several", true, false, "GET", []cacheFacts{ok(time.Hour), ok(0)}, "no-store"},
		{"negative ttl", true, false, "GET", []cacheFacts{ok(-time.Second)}, "no-store"},
		{"sub-second ttl", true, false, "GET", []cacheFacts{ok(500 * time.Millisecond)}, "no-store"},
		{"fractional ttl", true, false, "GET", []cacheFacts{ok(1500 * time.Millisecond)}, "no-store"},
		{"ttl above one year", true, false, "GET", []cacheFacts{ok(365*24*time.Hour + time.Second)}, "no-store"},
		{"policies on the only database", true, false, "GET", []cacheFacts{{TTL: time.Hour, HasAccessPolicies: true}}, "no-store"},
		{"policies on one of several", true, false, "GET", []cacheFacts{ok(time.Hour), {TTL: time.Hour, HasAccessPolicies: true}}, "no-store"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := relationalCacheControl(tc.readOnly, tc.auth, tc.method, tc.dbs)
			if got != tc.want {
				t.Fatalf("relationalCacheControl = %q, want %q", got, tc.want)
			}
		})
	}
}
