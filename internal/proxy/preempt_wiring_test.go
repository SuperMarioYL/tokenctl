package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/providers"
)

// wireAdmission is a controllable Admission whose Context can be cancelled to
// simulate an arbiter preempt, with Preempted() latched to match.
type wireAdmission struct {
	ctx    context.Context
	cancel context.CancelFunc
	pre    atomic.Bool
	rel    atomic.Bool
}

func newWireAdmission() *wireAdmission {
	ctx, cancel := context.WithCancel(context.Background())
	return &wireAdmission{ctx: ctx, cancel: cancel}
}

func (w *wireAdmission) GroupPath() string        { return "org.low" }
func (w *wireAdmission) AddInput(int64)           {}
func (w *wireAdmission) AddOutput(int64)          {}
func (w *wireAdmission) Release()                 { w.rel.Store(true) }
func (w *wireAdmission) Context() context.Context { return w.ctx }
func (w *wireAdmission) Preempted() bool          { return w.pre.Load() }

// preempt simulates the arbiter firing: latch Preempted then cancel the
// admission context (the same order budget.firePreempt uses).
func (w *wireAdmission) preempt() {
	w.pre.Store(true)
	w.cancel()
}

// wireTree hands out a fixed wireAdmission so the test controls preemption.
type wireTree struct{ adm *wireAdmission }

func (t *wireTree) Admit(string, string) (Admission, error) { return t.adm, nil }
func (t *wireTree) Snapshot() any                           { return struct{}{} }

// wireProvider points the proxy at a test upstream and recognises everything.
type wireProvider struct{ upstream *url.URL }

func (p *wireProvider) Name() string                           { return "claude" }
func (p *wireProvider) Upstream() *url.URL                     { return p.upstream }
func (p *wireProvider) Matches(*http.Request) bool             { return true }
func (p *wireProvider) APIKeyFromRequest(*http.Request) string { return "k" }
func (p *wireProvider) NewMeter() providers.Meter              { return &nopMeter{} }

type nopMeter struct{}

func (nopMeter) Observe(string, []byte) (int64, int64) { return 0, 0 }

// TestPreemptionCancelsUpstreamAndEmits499 is the regression for
// fix-preemption-not-wired-to-upstream: the arbiter cancelling the admission
// context must (a) tear down the in-flight upstream request and (b) make the
// client receive 499 + X-TokenCtl-Reason: preempted_by_sibling — not a normal
// 200 as in the pre-fix no-op behaviour.
func TestPreemptionCancelsUpstreamAndEmits499(t *testing.T) {
	upstreamCancelled := make(chan struct{}, 1)
	started := make(chan struct{})

	// Upstream holds the request open (no response headers yet) until its
	// request context is cancelled — exactly what a long agent completion that
	// hasn't started emitting looks like. Because no headers were committed,
	// the cancellation surfaces as a failed round-trip, letting the proxy emit
	// 499 to the client.
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			upstreamCancelled <- struct{}{}
		case <-time.After(5 * time.Second):
			t.Error("upstream was never cancelled — preemption did not reach the upstream call")
		}
	}))
	defer upstream.Close()

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

	// Fire the preempt shortly after the upstream stream starts.
	go func() {
		<-started
		time.Sleep(20 * time.Millisecond)
		adm.preempt()
	}()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	// (a) the upstream call must have been cancelled.
	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream context was not cancelled by the preempt")
	}

	// (b) the client sees 499 + the documented reason header.
	if resp.StatusCode != statusClientClosedRequest {
		t.Fatalf("client status = %d, want %d (499 Client Closed Request on preempt)",
			resp.StatusCode, statusClientClosedRequest)
	}
	if got := resp.Header.Get("X-TokenCtl-Reason"); got != "preempted_by_sibling" {
		t.Fatalf("X-TokenCtl-Reason = %q, want preempted_by_sibling", got)
	}
}

// TestNoPreemptionPassesThroughNormally is the negative control: with no
// preempt, a normal upstream response reaches the client as 200 and the
// preempt path is not taken.
func TestNoPreemptionPassesThroughNormally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

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

	resp, err := http.Get(proxySrv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200 (no preempt)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-TokenCtl-Reason"); got == "preempted_by_sibling" {
		t.Fatal("unexpected preempt reason on a non-preempted request")
	}
}
