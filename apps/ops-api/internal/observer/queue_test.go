package observer

import (
	"context"
	"testing"
	"time"
)

func TestAddPendingResolveWait(t *testing.T) {
	q := NewQueue(time.Minute)
	c := q.Add("r1", "Claude", KindShot, 0, 0)

	if q.Len() != 1 {
		t.Fatalf("len = %d, want 1", q.Len())
	}
	pv := q.Pending()
	if len(pv) != 1 || pv[0].ReqID != "r1" || pv[0].Target != "Claude" {
		t.Fatalf("pending = %+v", pv)
	}

	// Resolve from another goroutine; Wait should pick it up.
	go func() {
		time.Sleep(10 * time.Millisecond)
		if !q.Resolve("r1", Result{URL: "https://u", Pushed: []string{"telegram"}}) {
			t.Errorf("resolve returned false")
		}
	}()

	res, err := q.Wait(context.Background(), c)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.URL != "https://u" || len(res.Pushed) != 1 {
		t.Fatalf("res = %+v", res)
	}
	if q.Len() != 0 {
		t.Errorf("len after resolve = %d, want 0", q.Len())
	}
}

func TestResolveUnknownReqID(t *testing.T) {
	q := NewQueue(time.Minute)
	if q.Resolve("nope", Result{}) {
		t.Fatal("resolve of unknown reqId should return false")
	}
}

func TestWaitTimeoutRemoves(t *testing.T) {
	q := NewQueue(20 * time.Millisecond)
	c := q.Add("r1", "Claude", KindShot, 0, 0)
	_, err := q.Wait(context.Background(), c)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if q.Len() != 0 {
		t.Errorf("timed-out capture not removed, len = %d", q.Len())
	}
}

func TestWaitContextCancel(t *testing.T) {
	q := NewQueue(time.Minute)
	c := q.Add("r1", "Claude", KindShot, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Wait(ctx, c); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestTTLReap(t *testing.T) {
	q := NewQueue(30 * time.Millisecond)
	base := time.Now()
	q.now = func() time.Time { return base }
	q.Add("old", "A", KindShot, 0, 0)
	// Advance the clock past the TTL; cloud (via Pending) should reap it.
	q.now = func() time.Time { return base.Add(time.Second) }
	if pv := q.Pending(); len(pv) != 0 {
		t.Fatalf("expected reaped, got %+v", pv)
	}
}

// A clip's own TTL must win over the (short) queue default — recording plus
// encode plus upload easily outruns the 90s a screenshot needs.
func TestPerCaptureTTLOverridesQueueDefault(t *testing.T) {
	q := NewQueue(20 * time.Millisecond)
	base := time.Now()
	q.now = func() time.Time { return base }
	q.Add("clip", "Claude", KindClip, 15, time.Minute)
	q.now = func() time.Time { return base.Add(500 * time.Millisecond) }

	// Well past the 20ms queue default, well inside the capture's own minute.
	if pv := q.Pending(); len(pv) != 1 {
		t.Fatalf("clip reaped by the queue default TTL: %+v", pv)
	}
}

func TestPendingCarriesKindAndSeconds(t *testing.T) {
	q := NewQueue(time.Minute)
	q.Add("c1", "Claude", KindClip, 12, time.Minute)
	pv := q.Pending()
	if len(pv) != 1 || pv[0].Kind != KindClip || pv[0].Seconds != 12 {
		t.Fatalf("pending = %+v, want kind=clip seconds=12", pv)
	}
}

func TestKindOf(t *testing.T) {
	q := NewQueue(time.Minute)
	q.Add("c1", "Claude", KindClip, 10, 0)
	if k, ok := q.KindOf("c1"); !ok || k != KindClip {
		t.Fatalf("KindOf(c1) = %q, %v", k, ok)
	}
	if _, ok := q.KindOf("nope"); ok {
		t.Fatal("KindOf of unknown reqId should report not-found")
	}
}

// The async clip path never calls Wait, so Resolve must park the Result where
// get_observer_clip can Lookup it afterwards.
func TestResolveRetainsResultForLookup(t *testing.T) {
	q := NewQueue(time.Minute)
	q.Add("c1", "Claude", KindClip, 10, time.Minute)
	q.Resolve("c1", Result{URL: "https://u/clip.mp4", Pushed: []string{"slack"}})

	if q.Len() != 0 {
		t.Errorf("resolved capture still pending, len = %d", q.Len())
	}
	r, ok := q.Lookup("c1")
	if !ok || r.URL != "https://u/clip.mp4" {
		t.Fatalf("Lookup = %+v, %v", r, ok)
	}
}

func TestLookupExpiresAfterResultTTL(t *testing.T) {
	q := NewQueue(time.Minute)
	base := time.Now()
	q.now = func() time.Time { return base }
	q.Retain("c1", Result{URL: "https://u"})

	q.now = func() time.Time { return base.Add(resultTTL + time.Second) }
	if _, ok := q.Lookup("c1"); ok {
		t.Fatal("retained result outlived resultTTL")
	}
}
