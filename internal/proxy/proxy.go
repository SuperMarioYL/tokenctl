// Package proxy is the runtime that fronts inbound LLM traffic, attributes
// every streamed token to a leaf in the budget tree, and exposes Prometheus
// metrics + a /v1/snapshot endpoint that powers `tokenctl top`.
//
// The package owns two HTTP servers:
//
//   - the proxy listener (cfg.Listen) handles client traffic. Each request is
//     matched against a registered providers.Provider, admitted via the
//     budget tree, then forwarded to the upstream LLM API with the response
//     body wrapped in a meter that parses SSE events (or buffered JSON) for
//     token usage and reports deltas back to the admission ticket.
//
//   - the metrics listener (cfg.Metrics.Listen) serves Prometheus on
//     cfg.Metrics.Path and the snapshot endpoint `/v1/snapshot` consumed by
//     the `tokenctl top` CLI.
//
// The budget tree is supplied by the caller as a Tree. The proxy makes no
// scheduling decisions itself; it only enforces what Tree.Admit returns.
// m1 ships the metering + endpoints; m2 wires throttle / 429 deny; m3 wires
// in-flight preemption via Admission.Release on context cancellation.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/providers"
)

// Tree is the contract the proxy needs from the runtime budget tree.
//
// Admit pins an inbound api key + provider to a leaf and returns an
// Admission ticket the proxy uses for the lifetime of the request. The
// returned errors drive the proxy's HTTP response: ErrUnknownKey -> 401,
// ErrThrottled -> Retry-After 429 (m2), ErrDenied -> 429 with
// X-TokenCtl-Reason: budget_exceeded (m2). Snapshot returns whatever
// JSON-serialisable view powers `tokenctl top`; the proxy does not interpret
// its shape.
type Tree interface {
	Admit(apiKey, provider string) (Admission, error)
	Snapshot() any
}

// Admission is the per-request ticket Tree.Admit issues.
//
// GroupPath is the dotted tree path (e.g. "acme.team-platform.alice") used
// as a label on per-request metrics. AddInput / AddOutput report token
// deltas as the upstream response streams in; the implementation MUST be
// safe to call from the same goroutine that owns the response body reader.
// Release returns the in-flight slot to the tree and is called exactly once
// per ticket via defer.
type Admission interface {
	GroupPath() string
	AddInput(n int64)
	AddOutput(n int64)
	Release()

	// Context is cancelled when the arbiter preempts this request (or when
	// Release is called). The proxy injects it into the upstream call so a
	// preempt actually tears down the in-flight upstream stream
	// (fix-preemption-not-wired-to-upstream).
	Context() context.Context

	// Preempted reports whether the arbiter cancelled this admission, so the
	// proxy can distinguish a deliberate preempt from a client/upstream
	// disconnect and emit 499 + X-TokenCtl-Reason: preempted_by_sibling.
	Preempted() bool
}

// Store is the optional persistence layer. m1 doesn't require any methods;
// m3 will add an audit-log append point. Accepting it as a typed parameter
// today means main.go's signature stays stable across milestones.
type Store interface{}

// Tree.Admit error sentinels. The proxy package owns these so the budget
// package can return them by value without an import cycle.
var (
	// ErrUnknownKey signals an inbound credential is not bound to any leaf.
	ErrUnknownKey = errors.New("tokenctl: api key not bound to a tree leaf")

	// ErrThrottled signals a soft-throttle: the leaf is past its soft
	// threshold and the request should be delayed (m2 will queue, m1 simply
	// 429s).
	ErrThrottled = errors.New("tokenctl: leaf is soft-throttled")

	// ErrDenied signals a hard deny: the leaf is over its budget. The proxy
	// emits 429 with X-TokenCtl-Reason: budget_exceeded and the leaf path in
	// X-TokenCtl-Group.
	ErrDenied = errors.New("tokenctl: budget exceeded")
)

// statusClientClosedRequest is nginx's non-standard 499 "Client Closed
// Request". The proxy returns it to the client when the arbiter preempts an
// in-flight request, mirroring the m3 contract
// (fix-preemption-not-wired-to-upstream).
const statusClientClosedRequest = 499

// errPreempted is returned from ModifyResponse to route a preempted-but-already
// -responded upstream call into ErrorHandler so it emits 499 before any client
// bytes are written.
var errPreempted = errors.New("tokenctl: preempted")

// Server holds the runtime state. Construct with New, run with Run.
type Server struct {
	cfg       *config.Config
	store     Store
	tree      Tree
	providers []providers.Provider
	metrics   *metrics
	inFlight  atomic.Int64
	logger    *slog.Logger
}

// New assembles the runtime. It builds provider adapters from cfg.Providers,
// registers prometheus metrics, and (if the tree also implements
// prometheus.Collector) registers the tree's collectors against the same
// registry so /metrics scrapes a single coherent view.
func New(cfg *config.Config, store Store, tree Tree) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("proxy: nil config")
	}
	if tree == nil {
		return nil, errors.New("proxy: nil tree")
	}
	provs := make([]providers.Provider, 0, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		p, err := providers.Build(pc)
		if err != nil {
			return nil, fmt.Errorf("build provider %q: %w", pc.Name, err)
		}
		provs = append(provs, p)
	}
	m := newMetrics()
	if c, ok := tree.(prometheus.Collector); ok {
		m.Registry.MustRegister(c)
	}
	return &Server{
		cfg:       cfg,
		store:     store,
		tree:      tree,
		providers: provs,
		metrics:   m,
		logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, nil
}

// Run starts the proxy + metrics listeners and blocks until ctx is cancelled
// or either server fails. A graceful shutdown deadline of 5s is applied on
// teardown so in-flight upstream calls have a chance to drain.
func (s *Server) Run(ctx context.Context) error {
	proxySrv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.proxyHandler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              s.cfg.Metrics.Listen,
		Handler:           s.metricsHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		if s.cfg.TLS.CertFile != "" && s.cfg.TLS.KeyFile != "" {
			errCh <- proxySrv.ListenAndServeTLS(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
		} else {
			errCh <- proxySrv.ListenAndServe()
		}
	}()
	go func() { errCh <- metricsSrv.ListenAndServe() }()

	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)
	// Drain whichever ListenAndServe returned ErrServerClosed after Shutdown.
	for i := 0; i < 2; i++ {
		select {
		case <-errCh:
		case <-time.After(1 * time.Second):
		}
	}
	return runErr
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *Server) proxyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

func (s *Server) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(s.cfg.Metrics.Path, promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// handleSnapshot is consumed by `tokenctl top`. The body is whatever
// Tree.Snapshot returns; clients decode loosely so adding fields is safe.
func (s *Server) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(s.tree.Snapshot()); err != nil {
		s.logger.Error("encode snapshot", "err", err)
	}
}

// handleProxy is the hot path: match provider -> identify key -> admit ->
// reverse-proxy with metering.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	prov := s.matchProvider(r)
	if prov == nil {
		http.Error(w, "no tokenctl provider matches this path", http.StatusNotFound)
		return
	}

	apiKey := prov.APIKeyFromRequest(r)
	if apiKey == "" {
		w.Header().Set("X-TokenCtl-Reason", "missing_api_key")
		http.Error(w, "missing api key (expected Authorization: Bearer ... or x-api-key)", http.StatusUnauthorized)
		return
	}

	adm, err := s.tree.Admit(apiKey, prov.Name())
	if err != nil {
		s.writeAdmitError(w, err)
		return
	}
	defer adm.Release()

	groupPath := adm.GroupPath()
	s.inFlight.Add(1)
	s.metrics.InFlightGauge.Inc()
	defer func() {
		s.inFlight.Add(-1)
		s.metrics.InFlightGauge.Dec()
	}()

	started := time.Now()
	s.reverseProxy(prov, adm, w, r)
	s.metrics.DurationSeconds.WithLabelValues(prov.Name(), groupPath).Observe(time.Since(started).Seconds())
	s.metrics.RequestsTotal.WithLabelValues(prov.Name(), groupPath).Inc()
}

func (s *Server) matchProvider(r *http.Request) providers.Provider {
	for _, p := range s.providers {
		if p.Matches(r) {
			return p
		}
	}
	return nil
}

func (s *Server) writeAdmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownKey):
		w.Header().Set("X-TokenCtl-Reason", "unknown_key")
		http.Error(w, "api key not bound to any tree leaf — add it to api_keys in tokenctl.yaml", http.StatusUnauthorized)
	case errors.Is(err, ErrThrottled):
		w.Header().Set("X-TokenCtl-Reason", "soft_throttle")
		w.Header().Set("Retry-After", "10")
		http.Error(w, "tokenctl: leaf soft-throttled, retry shortly", http.StatusTooManyRequests)
	case errors.Is(err, ErrDenied):
		w.Header().Set("X-TokenCtl-Reason", "budget_exceeded")
		http.Error(w, "tokenctl: leaf budget exceeded", http.StatusTooManyRequests)
	default:
		s.logger.Error("admit failed", "err", err)
		http.Error(w, "tokenctl: admission failed", http.StatusInternalServerError)
	}
}

// reverseProxy wires httputil.ReverseProxy to the matched upstream with
// FlushInterval=-1 so SSE chunks reach the client immediately, and wraps the
// response body in a SSE-or-JSON metering reader before bytes are copied to
// the client.
func (s *Server) reverseProxy(prov providers.Provider, adm Admission, w http.ResponseWriter, r *http.Request) {
	upstream := prov.Upstream()
	rp := httputil.NewSingleHostReverseProxy(upstream)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = upstream.Host
		// Strip hop-by-hop headers that ReverseProxy doesn't already remove.
		req.Header.Del("X-Forwarded-For")
		req.Header.Set("X-Forwarded-Host", req.Host)
	}
	rp.FlushInterval = -1
	rp.ErrorLog = nil

	meter := prov.NewMeter()
	provName := prov.Name()

	rp.ModifyResponse = func(resp *http.Response) error {
		// If the arbiter preempted between dispatch and the upstream response
		// landing, abort here (before any client bytes) so ErrorHandler emits
		// 499 rather than streaming a now-unwanted 200
		// (fix-preemption-not-wired-to-upstream).
		if adm.Preempted() {
			_ = resp.Body.Close()
			return errPreempted
		}
		// Drop content-length when we may rewrap as a streaming reader so the
		// chunked-transfer path takes over cleanly.
		ct := resp.Header.Get("Content-Type")
		switch {
		case isSSE(ct):
			resp.Header.Del("Content-Length")
			resp.Body = newSSEMeteredReader(resp.Body, meter, adm, s.metrics, provName)
		case isJSON(ct):
			resp.Body = newJSONMeteredReader(resp.Body, meter, adm, s.metrics, provName)
		}
		// Stamp every upstream response with the leaf we attributed it to —
		// useful for debugging client-side why a particular request landed in
		// a particular bucket.
		resp.Header.Set("X-TokenCtl-Group", adm.GroupPath())
		resp.Header.Set("X-TokenCtl-Provider", provName)
		return nil
	}
	rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		// The arbiter cancels adm.Context() to preempt; that cancellation
		// surfaces here as a context error on the upstream round-trip. Map it
		// to 499 + X-TokenCtl-Reason: preempted_by_sibling so the client sees
		// a deliberate preempt, not a generic 502
		// (fix-preemption-not-wired-to-upstream).
		if adm.Preempted() {
			rw.Header().Set("X-TokenCtl-Reason", "preempted_by_sibling")
			rw.Header().Set("X-TokenCtl-Group", adm.GroupPath())
			rw.Header().Set("X-TokenCtl-Provider", provName)
			http.Error(rw, "tokenctl: request preempted by higher-weight sibling", statusClientClosedRequest)
			return
		}
		s.logger.Error("upstream error", "provider", provName, "err", err)
		rw.Header().Set("X-TokenCtl-Reason", "upstream_error")
		http.Error(rw, "tokenctl: upstream error", http.StatusBadGateway)
	}

	// Inject the admission's cancellable context into the upstream call so an
	// arbiter preempt actually tears the upstream stream down mid-flight
	// instead of letting it run to completion as a silent no-op
	// (fix-preemption-not-wired-to-upstream). We preserve the original request
	// context's values but swap its Done channel for the admission's.
	r = r.WithContext(mergeCancel(r.Context(), adm.Context()))

	rp.ServeHTTP(w, r)
}

// mergeCancel returns a context that carries parent's values but is cancelled
// when EITHER parent or cancel fires. This lets the upstream call honour both
// the client disconnecting (parent) and the arbiter preempting (cancel).
func mergeCancel(parent, cancel context.Context) context.Context {
	ctx, stop := context.WithCancel(parent)
	go func() {
		select {
		case <-cancel.Done():
			stop()
		case <-ctx.Done():
			stop()
		}
	}()
	return ctx
}

func isSSE(contentType string) bool {
	// Take the first MIME segment before any ";" parameter, lower-case it.
	if i := indexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return equalFold(contentType, "text/event-stream")
}

func isJSON(contentType string) bool {
	if i := indexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return equalFold(contentType, "application/json") || equalFold(contentType, "application/x-ndjson")
}

// Tiny stdlib-free helpers: keep the hot path free of strings.* allocs.

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Metering response-body wrappers
// ---------------------------------------------------------------------------

// sseMeteredReader splits the streamed body into SSE events (separated by
// "\n\n"), feeds event-name + data bytes to the Meter, and forwards the
// untouched bytes to the proxy's copy loop. This is the SSE half of m1.
type sseMeteredReader struct {
	src      io.ReadCloser
	meter    providers.Meter
	adm      Admission
	metrics  *metrics
	provider string

	pending bytes.Buffer
	mu      sync.Mutex
}

func newSSEMeteredReader(src io.ReadCloser, m providers.Meter, a Admission, mm *metrics, prov string) io.ReadCloser {
	return &sseMeteredReader{src: src, meter: m, adm: a, metrics: mm, provider: prov}
}

func (r *sseMeteredReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.consume(p[:n])
	}
	if errors.Is(err, io.EOF) {
		r.flush()
	}
	return n, err
}

func (r *sseMeteredReader) Close() error {
	r.flush()
	return r.src.Close()
}

func (r *sseMeteredReader) consume(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending.Write(chunk)
	for {
		buf := r.pending.Bytes()
		idx := bytes.Index(buf, []byte("\n\n"))
		if idx < 0 {
			return
		}
		event := append([]byte(nil), buf[:idx]...)
		r.pending.Next(idx + 2)
		r.processEvent(event)
	}
}

func (r *sseMeteredReader) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending.Len() == 0 {
		return
	}
	// Treat the trailing fragment as an event so usage in the last chunk of a
	// non-newline-terminated stream still lands.
	event := append([]byte(nil), r.pending.Bytes()...)
	r.pending.Reset()
	if len(bytes.TrimSpace(event)) > 0 {
		r.processEvent(event)
	}
}

func (r *sseMeteredReader) processEvent(raw []byte) {
	var (
		eventName string
		dataBuf   bytes.Buffer
	)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			// Per the SSE spec, multiple data: lines per event are joined with
			// "\n". Per-line leading single space (the spec's "one optional
			// space") is stripped.
			payload := line[len("data:"):]
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.Write(payload)
		}
	}
	if dataBuf.Len() == 0 {
		return
	}
	in, out := r.meter.Observe(eventName, dataBuf.Bytes())
	r.report(in, out)
}

func (r *sseMeteredReader) report(in, out int64) {
	if in > 0 {
		r.adm.AddInput(in)
		r.metrics.InputTokens.WithLabelValues(r.provider, r.adm.GroupPath()).Add(float64(in))
	}
	if out > 0 {
		r.adm.AddOutput(out)
		r.metrics.OutputTokens.WithLabelValues(r.provider, r.adm.GroupPath()).Add(float64(out))
	}
}

// maxUsageBufferBytes caps how many bytes a jsonMeteredReader retains in
// memory to locate the `usage` object on EOF
// (fix-jsonmeteredreader-buffers-entire-body). Previously the reader buffered
// the ENTIRE non-streamed body — a big embeddings batch or long buffered
// completion kept a full second copy of the body per in-flight request,
// scaling memory with concurrency×body (an OOM/DoS vector on the hot path)
// even though only the small trailing usage object is needed.
//
// 64 KiB comfortably holds the usage block of every provider shape we parse
// (Anthropic / OpenAI / Bedrock all emit `usage` near the END of the JSON
// object, after content), while keeping worst-case retained memory bounded
// regardless of body size.
const maxUsageBufferBytes = 64 << 10

// jsonMeteredReader copies a non-streamed JSON response to the client as bytes
// arrive (so latency isn't bumped) and parses usage on EOF. To bound memory it
// retains only a sliding TAIL of at most maxUsageBufferBytes — sufficient
// because every provider shape places `usage` at the end of the body — instead
// of the whole response.
type jsonMeteredReader struct {
	src      io.ReadCloser
	meter    providers.Meter
	adm      Admission
	metrics  *metrics
	provider string

	mu    sync.Mutex
	tail  []byte // last <= maxUsageBufferBytes bytes seen
	total int64  // total bytes observed (for diagnostics)
	done  atomic.Bool
}

func newJSONMeteredReader(src io.ReadCloser, m providers.Meter, a Admission, mm *metrics, prov string) io.ReadCloser {
	return &jsonMeteredReader{
		src:      src,
		meter:    m,
		adm:      a,
		metrics:  mm,
		provider: prov,
		tail:     make([]byte, 0, maxUsageBufferBytes),
	}
}

func (r *jsonMeteredReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.appendTail(p[:n])
	}
	if errors.Is(err, io.EOF) {
		r.finalize()
	}
	return n, err
}

// appendTail keeps only the last maxUsageBufferBytes bytes of the stream so
// per-request memory is O(cap) regardless of body size.
func (r *jsonMeteredReader) appendTail(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total += int64(len(chunk))
	if len(chunk) >= maxUsageBufferBytes {
		// This chunk alone overflows the window: keep only its tail.
		r.tail = append(r.tail[:0], chunk[len(chunk)-maxUsageBufferBytes:]...)
		return
	}
	r.tail = append(r.tail, chunk...)
	if len(r.tail) > maxUsageBufferBytes {
		// Drop the oldest bytes to stay within the cap.
		drop := len(r.tail) - maxUsageBufferBytes
		r.tail = append(r.tail[:0], r.tail[drop:]...)
	}
}

func (r *jsonMeteredReader) Close() error {
	r.finalize()
	return r.src.Close()
}

func (r *jsonMeteredReader) finalize() {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.mu.Lock()
	tail := r.tail
	total := r.total
	r.mu.Unlock()
	if len(tail) == 0 {
		return
	}

	// If the whole body fit inside the cap, the tail IS the complete response —
	// hand it to the meter verbatim. Otherwise the tail is a front-truncated
	// fragment that won't parse as JSON, so reconstruct a minimal valid
	// document carrying just the (top-level) usage signal the meters need
	// (fix-jsonmeteredreader-buffers-entire-body). usage always lives at the
	// END of the body, so it is present in the retained tail.
	body := tail
	if total > int64(len(tail)) {
		if reconstructed, ok := reconstructUsageJSON(tail); ok {
			body = reconstructed
		} else {
			// Couldn't locate a usage object in the tail (truncated mid-object
			// or an unmetered shape): skip attribution rather than feed the
			// meter invalid JSON. This matches the documented "what's metered"
			// limitation for exotic bodies.
			return
		}
	}

	in, out := r.meter.Observe("", body)
	if in > 0 {
		r.adm.AddInput(in)
		r.metrics.InputTokens.WithLabelValues(r.provider, r.adm.GroupPath()).Add(float64(in))
	}
	if out > 0 {
		r.adm.AddOutput(out)
		r.metrics.OutputTokens.WithLabelValues(r.provider, r.adm.GroupPath()).Add(float64(out))
	}
}

// reconstructUsageJSON scans a (possibly front-truncated) JSON fragment for the
// last top-level `"usage": { ... }` object and the Llama-style flat fields
// (prompt_token_count / generation_token_count) and returns a minimal valid
// JSON document the provider meters can parse. It returns ok=false when no
// usage signal is found. This is what lets the metered reader keep only a
// bounded tail of a large non-streamed body instead of the whole thing.
func reconstructUsageJSON(frag []byte) ([]byte, bool) {
	parts := make([][]byte, 0, 3)

	if obj := lastJSONObjectForKey(frag, "usage"); obj != nil {
		parts = append(parts, append([]byte(`"usage":`), obj...))
	}
	for _, k := range []string{"prompt_token_count", "generation_token_count"} {
		if v := lastJSONNumberForKey(frag, k); v != nil {
			parts = append(parts, append(append([]byte{'"'}, k...), append([]byte(`":`), v...)...))
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	out := bytes.Join(parts, []byte{','})
	doc := make([]byte, 0, len(out)+2)
	doc = append(doc, '{')
	doc = append(doc, out...)
	doc = append(doc, '}')
	return doc, true
}

// lastJSONObjectForKey finds the last occurrence of "key": and returns the
// balanced { ... } object that follows, or nil if the value isn't a complete
// object within frag.
func lastJSONObjectForKey(frag []byte, key string) []byte {
	needle := []byte(`"` + key + `"`)
	start := bytes.LastIndex(frag, needle)
	if start < 0 {
		return nil
	}
	i := start + len(needle)
	// Skip whitespace and the ':'.
	for i < len(frag) && (frag[i] == ' ' || frag[i] == '\t' || frag[i] == '\n' || frag[i] == '\r' || frag[i] == ':') {
		i++
	}
	if i >= len(frag) || frag[i] != '{' {
		return nil
	}
	depth := 0
	inStr := false
	esc := false
	for j := i; j < len(frag); j++ {
		c := frag[j]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return frag[i : j+1]
			}
		}
	}
	return nil // unbalanced — object truncated by the tail window
}

// lastJSONNumberForKey finds the last "key": <number> and returns the number
// literal, or nil if absent / not a bare number.
func lastJSONNumberForKey(frag []byte, key string) []byte {
	needle := []byte(`"` + key + `"`)
	start := bytes.LastIndex(frag, needle)
	if start < 0 {
		return nil
	}
	i := start + len(needle)
	for i < len(frag) && (frag[i] == ' ' || frag[i] == '\t' || frag[i] == '\n' || frag[i] == '\r' || frag[i] == ':') {
		i++
	}
	j := i
	for j < len(frag) {
		c := frag[j]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			j++
			continue
		}
		break
	}
	if j == i {
		return nil
	}
	return frag[i:j]
}

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

type metrics struct {
	Registry        *prometheus.Registry
	RequestsTotal   *prometheus.CounterVec
	InputTokens     *prometheus.CounterVec
	OutputTokens    *prometheus.CounterVec
	DurationSeconds *prometheus.HistogramVec
	InFlightGauge   prometheus.Gauge
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		Registry: reg,
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "tokenctl",
				Name:      "requests_total",
				Help:      "Total proxied requests, by upstream provider and tree leaf.",
			},
			[]string{"provider", "group"},
		),
		InputTokens: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "tokenctl",
				Name:      "input_tokens_total",
				Help:      "Total input tokens metered from upstream usage, by provider and tree leaf.",
			},
			[]string{"provider", "group"},
		),
		OutputTokens: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "tokenctl",
				Name:      "output_tokens_total",
				Help:      "Total output tokens metered from upstream usage, by provider and tree leaf.",
			},
			[]string{"provider", "group"},
		),
		DurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "tokenctl",
				Name:      "request_duration_seconds",
				Help:      "Wall-clock latency from admission to response close.",
				Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
			},
			[]string{"provider", "group"},
		),
		InFlightGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "tokenctl",
			Name:      "requests_in_flight",
			Help:      "Number of requests currently held by the proxy.",
		}),
	}
	reg.MustRegister(
		m.RequestsTotal,
		m.InputTokens,
		m.OutputTokens,
		m.DurationSeconds,
		m.InFlightGauge,
	)
	return m
}

// upstreamHostLabel renders host:port for metric label cardinality. Kept
// here (rather than per-request) so future cases that want a provider+host
// breakdown can call it without re-parsing.
//
//nolint:unused // reserved for m3 multi-region upstreams
func upstreamHostLabel(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Host
}
