package hue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// putLog is one PUT the test bridge received.
type putLog struct {
	Path       string
	Body       string
	Start, End time.Time
}

// testBridge is an in-process stand-in for test/mock_bridge with a per-PUT
// delay, a failing target and a record of when each PUT started and ended.
type testBridge struct {
	srv      *httptest.Server
	delay    time.Duration
	failPath string // a PUT to this path answers 400 (after the delay)

	mu       sync.Mutex
	puts     []putLog
	inFlight int
	maxIn    int
}

func newTestBridge(t *testing.T, delay time.Duration, lights int) *testBridge {
	t.Helper()
	b := &testBridge{delay: delay}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			var data []map[string]any
			if strings.HasSuffix(r.URL.Path, "/resource/light") {
				for i := range lights {
					data = append(data, map[string]any{"id": fmt.Sprintf("l%d", i), "on": map[string]any{"on": false},
						"dimming": map[string]any{"brightness": 50.0}})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{}, "data": data})
			return
		}
		var body strings.Builder
		buf := make([]byte, 512)
		for {
			n, err := r.Body.Read(buf)
			body.Write(buf[:n])
			if err != nil {
				break
			}
		}
		start := time.Now()
		b.mu.Lock()
		b.inFlight++
		b.maxIn = max(b.maxIn, b.inFlight)
		b.mu.Unlock()
		time.Sleep(b.delay)
		b.mu.Lock()
		b.inFlight--
		b.puts = append(b.puts, putLog{Path: r.URL.Path, Body: body.String(), Start: start, End: time.Now()})
		b.mu.Unlock()
		if r.URL.Path == b.failPath {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"description":"device (x) does not support color"}],"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"errors":[],"data":[]}`))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *testBridge) conn() BridgeConnection {
	return BridgeConnection{BaseURL: b.srv.URL, AppKey: "test-key"}
}

// log returns the PUTs ordered by start time.
func (b *testBridge) log() []putLog {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]putLog(nil), b.puts...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func lightReqs(n int) []stagedRequest {
	reqs := make([]stagedRequest, n)
	for i := range reqs {
		reqs[i] = stagedRequest{Kind: "light", Target: fmt.Sprintf("l%d", i), Body: omap{{"on", omap{{"on", true}}}}}
	}
	return reqs
}

// Slack for the server-side start gaps: pace() spaces client starts by
// lightCommandGap, and the server sees them a little jittered.
const gapSlack = 25 * time.Millisecond

func checkStartGaps(t *testing.T, puts []putLog, gap time.Duration) {
	t.Helper()
	for i := 1; i < len(puts); i++ {
		if d := puts[i].Start.Sub(puts[i-1].Start); d < gap-gapSlack {
			t.Errorf("PUT %d started %s after PUT %d; the pace gap is %s", i, d, i-1, gap)
		}
	}
}

func TestSendAllPipelinesLights(t *testing.T) {
	const n, delay = 12, 300 * time.Millisecond
	b := newTestBridge(t, delay, n)
	pace("light") // start from a fresh pace slot
	t0 := time.Now()
	sent, err := sendAll(b.conn(), lightReqs(n))
	elapsed := time.Since(t0)
	if err != nil || sent != n {
		t.Fatalf("sendAll = %d, %v; want %d, nil", sent, err, n)
	}
	t.Logf("%d light PUTs with a %s delay took %s (sequential would be ~%s)",
		n, delay, elapsed, time.Duration(n)*delay)
	// Paced starts: (n-1) gaps, plus the last request's own delay.
	floor := time.Duration(n-1)*lightCommandGap + delay - 2*gapSlack
	if elapsed < floor {
		t.Errorf("took %s, faster than the pacing allows (%s)", elapsed, floor)
	}
	if ceiling := time.Duration(n) * delay * 6 / 10; elapsed > ceiling {
		t.Errorf("took %s, not pipelined (want well under %d x %s, ceiling %s)", elapsed, n, delay, ceiling)
	}
	puts := b.log()
	if len(puts) != n {
		t.Fatalf("bridge got %d PUTs, want %d", len(puts), n)
	}
	checkStartGaps(t, puts, lightCommandGap)
}

func TestSendAllCapsInFlight(t *testing.T) {
	// A delay much longer than 4 gaps: without the cap, all 8 would overlap.
	b := newTestBridge(t, 700*time.Millisecond, 8)
	if _, err := sendAll(b.conn(), lightReqs(8)); err != nil {
		t.Fatal(err)
	}
	if b.maxIn != maxInFlightRequests {
		t.Errorf("max requests in flight = %d, want %d", b.maxIn, maxInFlightRequests)
	}
}

func TestSendAllKeepsOrderPerTarget(t *testing.T) {
	b := newTestBridge(t, 200*time.Millisecond, 3)
	// One UPDATE row that changes state and renames l0 produces two PUTs.
	var reqs []stagedRequest
	for _, body := range updateRequests(omap{{"on", omap{{"on", true}}}, {"metadata", omap{{"name", "New"}}}}) {
		reqs = append(reqs, stagedRequest{Kind: "light", Target: "l0", Body: body})
	}
	if len(reqs) != 2 {
		t.Fatalf("updateRequests gave %d requests, want 2", len(reqs))
	}
	reqs = append(reqs, stagedRequest{Kind: "light", Target: "l1", Body: omap{{"on", omap{{"on", true}}}}},
		stagedRequest{Kind: "light", Target: "l0", Body: omap{{"on", omap{{"on", false}}}}})
	if _, err := sendAll(b.conn(), reqs); err != nil {
		t.Fatal(err)
	}
	var l0 []putLog
	for _, p := range b.log() {
		if strings.HasSuffix(p.Path, "/l0") {
			l0 = append(l0, p)
		}
	}
	want := []string{`{"on":{"on":true}}`, `{"metadata":{"name":"New"}}`, `{"on":{"on":false}}`}
	if len(l0) != len(want) {
		t.Fatalf("l0 got %d PUTs, want %d", len(l0), len(want))
	}
	for i, p := range l0 {
		if p.Body != want[i] {
			t.Errorf("l0 PUT %d = %s, want %s", i, p.Body, want[i])
		}
		if i > 0 && p.Start.Before(l0[i-1].End) {
			t.Errorf("l0 PUT %d started before PUT %d completed", i, i-1)
		}
	}
}

func TestSendAllGroupIsBarrier(t *testing.T) {
	b := newTestBridge(t, 0, 2)
	b.delay = 150 * time.Millisecond
	reqs := append(lightReqs(2), stagedRequest{Kind: "grouped_light", Target: "g1", Body: omap{{"on", omap{{"on", false}}}}})
	reqs = append(reqs, lightReqs(1)...)
	if _, err := sendAll(b.conn(), reqs); err != nil {
		t.Fatal(err)
	}
	puts := b.log()
	var g putLog
	for _, p := range puts {
		if strings.Contains(p.Path, "grouped_light") {
			g = p
		}
	}
	for _, p := range puts {
		if p.Path == g.Path {
			continue
		}
		if p.Start.Before(g.Start) && p.End.After(g.Start) {
			t.Errorf("%s was still in flight when the group command started", p.Path)
		}
		if p.Start.After(g.Start) && p.Start.Before(g.End) {
			t.Errorf("%s started while the group command was in flight", p.Path)
		}
	}
}

func TestSendAllStopsOnFirstError(t *testing.T) {
	const n = 12
	// 350 ms is not a multiple of the 100 ms gap, so the failure never lands
	// on a start slot: l2 fails at ~550 ms, between the l5 and l6 slots.
	b := newTestBridge(t, 350*time.Millisecond, n)
	b.failPath = "/clip/v2/resource/light/l2"
	sent, err := sendAll(b.conn(), lightReqs(n))
	returned := time.Now()
	want := "PUT /resource/light/l2 failed with HTTP 400: device (x) does not support color"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if _, ok := err.(*HueError); !ok {
		t.Errorf("err is %T, want *HueError", err)
	}
	puts := b.log()
	var failedAt time.Time
	for _, p := range puts {
		if p.Path == b.failPath {
			failedAt = p.End
		}
		if p.End.After(returned) {
			t.Errorf("%s was still in flight when sendAll returned", p.Path)
		}
	}
	for _, p := range puts {
		if p.Start.After(failedAt) {
			t.Errorf("%s started after the failure", p.Path)
		}
	}
	// By the time l2 fails, l3..l5 are in flight; they finish and count.
	if len(puts) >= n || sent != len(puts)-1 {
		t.Errorf("bridge got %d PUTs, sendAll reported %d sent; want fewer than %d, all but the failure", len(puts), sent, n)
	}
	t.Logf("failure on the 3rd of %d: %d PUTs sent, %d succeeded", n, len(puts), sent)
}

func TestCommitReplayIsPipelined(t *testing.T) {
	t.Setenv("HUE_TX_DIR", t.TempDir())
	const n, delay = 12, 300 * time.Millisecond
	b := newTestBridge(t, delay, n)
	tx := []byte("pipelined-commit-1")
	if err := stage(tx, lightReqs(n)); err != nil {
		t.Fatal(err)
	}
	opts := BridgeOptions{Bridge: b.srv.URL, AppKey: "test-key", Transactional: true}
	t0 := time.Now()
	if err := commitTransaction(opts, tx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(t0)
	t.Logf("COMMIT of %d staged light PUTs with a %s delay took %s", n, delay, elapsed)
	if ceiling := time.Duration(n) * delay * 6 / 10; elapsed > ceiling {
		t.Errorf("COMMIT took %s, not pipelined (ceiling %s)", elapsed, ceiling)
	}
	puts := b.log()
	if len(puts) != n {
		t.Fatalf("bridge got %d PUTs, want %d", len(puts), n)
	}
	checkStartGaps(t, puts, lightCommandGap)
}

func TestCommitReplayFailureRestores(t *testing.T) {
	t.Setenv("HUE_TX_DIR", t.TempDir())
	b := newTestBridge(t, 350*time.Millisecond, 6)
	b.failPath = "/clip/v2/resource/light/l1"
	tx := []byte("pipelined-commit-2")
	if err := stage(tx, lightReqs(6)); err != nil {
		t.Fatal(err)
	}
	err := commitTransaction(BridgeOptions{Bridge: b.srv.URL, AppKey: "test-key", Transactional: true}, tx)
	// l1 fails at ~450 ms: l0 and l2..l4 (in flight) succeed, l5 never starts.
	want := "COMMIT failed after 4 of 6 light changes: PUT /resource/light/l1 failed with HTTP 400: " +
		"device (x) does not support color. Restored 5 light(s) to their state before the commit." // l1 refuses its restore too
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if puts := b.log(); len(puts) != 5+6 {
		t.Errorf("bridge got %d PUTs, want 5 commit + 6 restore", len(puts))
	}
}
