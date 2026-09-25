package mcpserver

import "testing"

func TestRateLimiterDefault(t *testing.T) {
	rl := newRateLimiter(map[string]int{}, 3)
	for i := 0; i < 3; i++ {
		if !rl.allow("any", "198.51.100.4") {
			t.Fatalf("expected allow on call %d", i)
		}
	}
	if rl.allow("any", "198.51.100.4") {
		t.Fatal("expected deny after default cap (3) hit")
	}
	// Different IP gets its own bucket.
	if !rl.allow("any", "198.51.100.8") {
		t.Fatal("expected separate bucket per IP")
	}
}

func TestRateLimiterPerToolOverride(t *testing.T) {
	rl := newRateLimiter(map[string]int{"db_exec": 1}, 30)
	if !rl.allow("db_exec", "198.51.100.4") {
		t.Fatal("expected first call to allow")
	}
	if rl.allow("db_exec", "198.51.100.4") {
		t.Fatal("expected db_exec=1 cap to deny second call")
	}
	// Other tools use the default.
	for i := 0; i < 5; i++ {
		if !rl.allow("db_query", "198.51.100.4") {
			t.Fatalf("expected db_query default cap to allow call %d", i)
		}
	}
}
