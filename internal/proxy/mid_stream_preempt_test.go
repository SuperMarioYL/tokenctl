package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/providers"
)

// testCounterValue reads the current value of one label series of a CounterVec
// via the metric's own Write, avoiding any new module dependency.
func testCounterValue(t *testing.T, cv *prometheus.CounterVec, label string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("get counter %q: %v", label, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write counter %q: %v", label, err)
	}
	return m.GetCounter().GetValue()
}

// TestSSEMeteredReader_MidStreamPreemptInjectsErrorFrame is the unit-level
// regression for fix-mid-stream-preempt-surfaces-truncated-200-not-499. Once
// bytes have already been forwarded to the client (headers flushed) a preempt
// can no longer rewrite the 200 status line to 499, so the reader must instead
// inject a terminating `event: error` SSE frame carrying
// reason=preempted_by_sibling and then fail the copy with a non-EOF error — NOT
// a clean EOF that reads as a normal short completion.
func TestSSEMeteredReader_MidStreamPreemptInjectsErrorFrame(t *testing.T) {
	// A source that yields one normal SSE event, then blocks so the test can
	// preempt after the first chunk has been forwarded (headers committed).
	src := &blockingSSESource{
		first: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"),
		block: make(chan struct{}),
	}
	adm := newFakeAdmission()
	m := newMetrics()
	rc := newSSEMeteredReader(src, &nopMeter{}, adm, m, "claude").(*sseMeteredReader)

	var fired atomic.Bool
	rc.onMidStreamPreempt = func() { fired.Store(true) }

	var client bytes.Buffer
	buf := make([]byte, 4096)

	// Read 1: forwards the first upstream event, latching streamed==true.
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatalf("read 1: unexpected error %v", err)
	}
	client.Write(buf[:n])
	if !rc.streamed.Load() {
		t.Fatal("streamed not latched after first byte forwarded")
	}

	// Now the arbiter preempts mid-stream.
	adm.pre.Store(true)

	// Read 2: must inject the error frame and return a non-EOF error.
	n, err = rc.Read(buf)
	client.Write(buf[:n])
	if err == nil || err == io.EOF {
		t.Fatalf("mid-stream read returned err=%v, want a non-nil non-EOF error", err)
	}
	if err != errMidStreamPreempt {
		t.Fatalf("mid-stream read err = %v, want errMidStreamPreempt", err)
	}
	if !fired.Load() {
		t.Fatal("onMidStreamPreempt callback was not invoked")
	}

	out := client.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("client stream missing terminating error event:\n%q", out)
	}
	if !strings.Contains(out, midStreamPreemptData) {
		t.Fatalf("client stream missing preempt reason payload %q:\n%q", midStreamPreemptData, out)
	}
	close(src.block)
}

// TestSSEMeteredReader_MidStreamPreemptSmallBuffer verifies the injected frame
// is delivered in full even when the client's read buffer is smaller than the
// frame, and the terminating error still fires after the remainder drains.
func TestSSEMeteredReader_MidStreamPreemptSmallBuffer(t *testing.T) {
	src := &blockingSSESource{
		first: []byte("data: {}\n\n"),
		block: make(chan struct{}),
	}
	defer close(src.block)
	adm := newFakeAdmission()
	rc := newSSEMeteredReader(src, &nopMeter{}, adm, newMetrics(), "claude").(*sseMeteredReader)

	small := make([]byte, 8) // smaller than midStreamPreemptEvent

	// Prime the stream so streamed latches.
	if _, err := rc.Read(make([]byte, 4096)); err != nil {
		t.Fatalf("prime read: %v", err)
	}
	adm.pre.Store(true)

	var got bytes.Buffer
	var lastErr error
	for i := 0; i < 100; i++ {
		n, err := rc.Read(small)
		got.Write(small[:n])
		lastErr = err
		if err != nil {
			break
		}
	}
	if lastErr != errMidStreamPreempt {
		t.Fatalf("final err = %v, want errMidStreamPreempt", lastErr)
	}
	if !bytes.Equal(got.Bytes(), midStreamPreemptEvent) {
		t.Fatalf("reassembled frame = %q, want %q", got.Bytes(), midStreamPreemptEvent)
	}
}

// TestMidStreamPreemptEndToEnd drives the full reverse proxy: a streaming SSE
// upstream flushes headers + a first event (committing the 200 status line),
// the arbiter then preempts, and the client must observe the injected
// `event: error` terminating frame AND a non-graceful stream close — proving a
// mid-stream preempt is no longer an undetectable truncated-200.
func TestMidStreamPreemptEndToEnd(t *testing.T) {
	firstFlushed := make(chan struct{})
	releaseUpstream := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		fl, ok := rw.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		// First event: forces headers + status line onto the wire.
		_, _ = io.WriteString(rw, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fl.Flush()
		close(firstFlushed)
		// Hold the stream open until the proxy tears us down (preempt) or the
		// test releases us; do NOT send a clean terminating chunk.
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	upURL, _ := url.Parse(upstream.URL)
	adm := newWireAdmission()
	s := &Server{
		cfg:       &config.Config{Listen: ":0", Metrics: config.MetricsConfig{Listen: ":0", Path: "/metrics"}},
		tree:      &wireTree{adm: adm},
		providers: []providers.Provider{&wireProvider{upstream: upURL}},
		metrics:   newMetrics(),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	proxySrv := httptest.NewServer(s.proxyHandler())
	defer proxySrv.Close()

	// Preempt only AFTER the first event has been flushed to the client, so this
	// exercises the mid-stream path (headers already committed), not the
	// pre-header 499 path.
	go func() {
		<-firstFlushed
		time.Sleep(20 * time.Millisecond)
		adm.preempt()
	}()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close()

	// Status is 200 — that's the whole point of the mid-stream case; the status
	// line was already committed before the preempt could change it.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers were flushed pre-preempt)", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	sbody := string(body)
	// The client must see BOTH the original event and the injected terminating
	// error frame — the signal that makes the preempt detectable.
	if !strings.Contains(sbody, "message_start") {
		t.Fatalf("client never received the first upstream event:\n%q", sbody)
	}
	if !strings.Contains(sbody, "event: error") || !strings.Contains(sbody, "preempted_by_sibling") {
		t.Fatalf("client body missing the mid-stream preempt error frame:\n%q", sbody)
	}

	// And it must be observable that the mid-stream path (not pre-header) fired.
	if got := testCounterValue(t, s.metrics.PreemptSignals, "mid_stream"); got != 1 {
		t.Fatalf("mid_stream preempt signal counter = %v, want 1", got)
	}
	if got := testCounterValue(t, s.metrics.PreemptSignals, "pre_header"); got != 0 {
		t.Fatalf("pre_header preempt signal counter = %v, want 0 (this was a mid-stream preempt)", got)
	}
}

// blockingSSESource yields `first` on the initial Read, then blocks on `block`
// so a test can interleave a preempt between the first forwarded chunk and the
// next Read.
type blockingSSESource struct {
	first  []byte
	sent   bool
	block  chan struct{}
	closed atomic.Bool
}

func (b *blockingSSESource) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, b.first)
		return n, nil
	}
	<-b.block
	return 0, io.EOF
}

func (b *blockingSSESource) Close() error {
	b.closed.Store(true)
	return nil
}

// oneChunkThenEOFSource yields `first` on the initial Read (n>0, nil) then
// returns io.EOF on every subsequent Read. It stands in for an upstream
// connection torn down by an arbiter preempt: with the JSON mid-stream-preempt
// fix the reader's top guard intercepts Read 2 before this source is touched,
// and without the fix Read 2 returns io.EOF — the silent truncated-200 the fix
// closes (a clean EOF indistinguishable from a normal short completion). Using
// a non-blocking source keeps the regression test revert-safe (no hang when the
// fix is reverted and the top guard is gone).
type oneChunkThenEOFSource struct {
	first []byte
	sent  bool
}

func (s *oneChunkThenEOFSource) Read(p []byte) (int, error) {
	if !s.sent {
		s.sent = true
		return copy(p, s.first), nil
	}
	return 0, io.EOF
}

func (s *oneChunkThenEOFSource) Close() error { return nil }

// TestJSONMeteredReader_MidStreamPreemptAbortsNonGraceful is the JSON-path
// regression for fix-json-mid-stream-preempt-not-surfaced, mirroring
// TestSSEMeteredReader_MidStreamPreemptInjectsErrorFrame over the JSON metered
// reader. Once bytes have already been forwarded to the client (headers
// flushed) a preempt can no longer rewrite the 200 status line to 499, and
// unlike SSE there is no body frame to inject — so the reader must instead fire
// the mid_stream counter and fail the copy with a non-EOF error, yielding a
// truncated body the client fails to parse rather than a clean completion (NOT
// a silent truncated 200). Must FAIL without the fix (clean EOF / silent
// truncation), PASS with.
func TestJSONMeteredReader_MidStreamPreemptAbortsNonGraceful(t *testing.T) {
	// A source that yields one chunk of a JSON body, then EOF, so the test can
	// preempt after the first chunk has been forwarded (headers committed).
	src := &oneChunkThenEOFSource{
		first: []byte(`{"id":"abc","data":"partial-body-still-arriving`),
	}
	adm := newFakeAdmission()
	m := newMetrics()
	rc := newJSONMeteredReader(src, &nopMeter{}, adm, m, "openai").(*jsonMeteredReader)

	var fired atomic.Bool
	// Mirror how reverseProxy wires the SSE reader's callback: the mid_stream
	// PreemptSignals counter is incremented exactly once when the preempt is
	// surfaced.
	rc.onMidStreamPreempt = func() {
		fired.Store(true)
		m.PreemptSignals.WithLabelValues("mid_stream").Inc()
	}

	var client bytes.Buffer
	buf := make([]byte, 4096)

	// Read 1: forwards the first upstream chunk, latching streamed==true (the
	// 200 status line + headers are now irrevocably committed).
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatalf("read 1: unexpected error %v", err)
	}
	client.Write(buf[:n])
	if !rc.streamed.Load() {
		t.Fatal("streamed not latched after first byte forwarded")
	}

	// Now the arbiter preempts mid-stream.
	adm.pre.Store(true)

	// Read 2: must surface the preempt as a non-EOF error (not a silent
	// truncated 200) and fire the mid_stream counter callback. Without the fix
	// this Read returns io.EOF (a clean, undetectable short completion).
	n, err = rc.Read(buf)
	client.Write(buf[:n])
	if err == nil || err == io.EOF {
		t.Fatalf("mid-stream read returned err=%v, want a non-nil non-EOF error", err)
	}
	if err != errMidStreamPreempt {
		t.Fatalf("mid-stream read err = %v, want errMidStreamPreempt", err)
	}
	if !fired.Load() {
		t.Fatal("onMidStreamPreempt callback was not invoked")
	}
	// The preempt must be observable: the mid_stream counter (not pre_header)
	// increments — the same signal the SSE mid-stream path emits.
	if got := testCounterValue(t, m.PreemptSignals, "mid_stream"); got != 1 {
		t.Fatalf("mid_stream preempt signal counter = %v, want 1", got)
	}
	if got := testCounterValue(t, m.PreemptSignals, "pre_header"); got != 0 {
		t.Fatalf("pre_header preempt signal counter = %v, want 0 (this was a mid-stream preempt)", got)
	}
}
