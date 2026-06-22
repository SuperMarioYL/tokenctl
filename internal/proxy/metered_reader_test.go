package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SuperMarioYL/tokenctl/internal/providers"
)

// fakeAdmission is a no-op Admission that records token deltas for assertions.
type fakeAdmission struct {
	in  atomic.Int64
	out atomic.Int64
	ctx context.Context
	pre atomic.Bool
}

func newFakeAdmission() *fakeAdmission {
	return &fakeAdmission{ctx: context.Background()}
}

func (f *fakeAdmission) GroupPath() string        { return "org.team.dev" }
func (f *fakeAdmission) AddInput(n int64)         { f.in.Add(n) }
func (f *fakeAdmission) AddOutput(n int64)        { f.out.Add(n) }
func (f *fakeAdmission) Release()                 {}
func (f *fakeAdmission) Context() context.Context { return f.ctx }
func (f *fakeAdmission) Preempted() bool          { return f.pre.Load() }

// usageMeter parses a top-level {"usage":{"input_tokens","output_tokens"}}
// shape — the same contract the real provider meters honour for buffered JSON.
type usageMeter struct{ calls int }

func (m *usageMeter) Observe(_ string, data []byte) (int64, int64) {
	m.calls++
	var env struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, 0
	}
	return env.Usage.InputTokens, env.Usage.OutputTokens
}

// drainReader copies a metered reader exactly as the proxy's copy loop would,
// then closes it, returning the bytes forwarded to the client.
func drainReader(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	var out bytes.Buffer
	// Small buffer so a large body exercises the multi-Read tail-sliding path.
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return out.Bytes()
}

// TestJSONMeteredReader_SmallBodyUsageReadCorrectly is the baseline: a small
// buffered response that fits inside the cap is metered exactly and forwarded
// byte-for-byte to the client.
func TestJSONMeteredReader_SmallBodyUsageReadCorrectly(t *testing.T) {
	body := []byte(`{"id":"abc","content":"hello world","usage":{"input_tokens":42,"output_tokens":17}}`)
	adm := newFakeAdmission()
	m := &metrics{}
	*m = *newMetrics()
	rc := newJSONMeteredReader(io.NopCloser(bytes.NewReader(body)), &usageMeter{}, adm, m, "claude")

	got := drainReader(t, rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("client bytes altered:\n got %q\nwant %q", got, body)
	}
	if adm.in.Load() != 42 || adm.out.Load() != 17 {
		t.Fatalf("metered in=%d out=%d, want 42/17", adm.in.Load(), adm.out.Load())
	}
}

// TestJSONMeteredReader_BoundedBufferOnHugeBody is the regression for
// fix-jsonmeteredreader-buffers-entire-body: a multi-megabyte buffered
// response must (a) be forwarded to the client in full, (b) still yield the
// correct usage, and (c) NOT retain the whole body in memory — the reader's
// tail must stay within maxUsageBufferBytes regardless of body size.
func TestJSONMeteredReader_BoundedBufferOnHugeBody(t *testing.T) {
	// Build a body far larger than the cap with usage at the very end (as every
	// provider emits it).
	var sb strings.Builder
	sb.WriteString(`{"id":"big","data":"`)
	filler := strings.Repeat("x", 4<<20) // 4 MiB of content
	sb.WriteString(filler)
	sb.WriteString(`","usage":{"input_tokens":900000,"output_tokens":1234567}}`)
	body := []byte(sb.String())
	if len(body) <= maxUsageBufferBytes {
		t.Fatalf("test body (%d) must exceed cap (%d)", len(body), maxUsageBufferBytes)
	}

	adm := newFakeAdmission()
	m := newMetrics()
	jr := newJSONMeteredReader(io.NopCloser(bytes.NewReader(body)), &usageMeter{}, adm, m, "claude")
	concrete := jr.(*jsonMeteredReader)

	got := drainReader(t, jr)

	// (a) client received every byte.
	if !bytes.Equal(got, body) {
		t.Fatalf("client did not receive the full body: got %d bytes, want %d", len(got), len(body))
	}
	// (b) usage still correct despite never buffering the whole body.
	if adm.in.Load() != 900000 || adm.out.Load() != 1234567 {
		t.Fatalf("metered in=%d out=%d, want 900000/1234567", adm.in.Load(), adm.out.Load())
	}
	// (c) retained memory bounded.
	concrete.mu.Lock()
	retained := len(concrete.tail)
	total := concrete.total
	concrete.mu.Unlock()
	if retained > maxUsageBufferBytes {
		t.Fatalf("retained tail = %d bytes, want <= cap %d (buffer not bounded)", retained, maxUsageBufferBytes)
	}
	if total != int64(len(body)) {
		t.Fatalf("total observed = %d, want %d", total, len(body))
	}
}

// TestJSONMeteredReader_BoundedBufferAcrossManyReads ensures the tail-sliding
// logic is correct when the body arrives in many small chunks (the realistic
// streaming-copy case), not one giant Read.
func TestJSONMeteredReader_BoundedBufferAcrossManyReads(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"n":0,`)
	for i := 0; i < 5000; i++ { // pad well past the cap
		sb.WriteString(fmt.Sprintf(`"k%d":"vvvvvvvvvvvvvvvvvvvv",`, i))
	}
	sb.WriteString(`"usage":{"input_tokens":7,"output_tokens":9}}`)
	body := []byte(sb.String())

	adm := newFakeAdmission()
	m := newMetrics()
	jr := newJSONMeteredReader(io.NopCloser(bytes.NewReader(body)), &usageMeter{}, adm, m, "openai-shaped")
	concrete := jr.(*jsonMeteredReader)

	got := drainReader(t, jr)
	if !bytes.Equal(got, body) {
		t.Fatal("client bytes altered across chunked reads")
	}
	if adm.in.Load() != 7 || adm.out.Load() != 9 {
		t.Fatalf("metered in=%d out=%d, want 7/9", adm.in.Load(), adm.out.Load())
	}
	concrete.mu.Lock()
	retained := len(concrete.tail)
	concrete.mu.Unlock()
	if len(body) > maxUsageBufferBytes && retained > maxUsageBufferBytes {
		t.Fatalf("retained %d > cap %d", retained, maxUsageBufferBytes)
	}
}

// TestReconstructUsageJSON_ExtractsTailObject unit-tests the tail extraction
// directly: a front-truncated fragment that is NOT valid JSON must still yield
// a parseable {"usage":...} document.
func TestReconstructUsageJSON_ExtractsTailObject(t *testing.T) {
	frag := []byte(`runaway...","content":"truncated front","usage":{"input_tokens":11,"output_tokens":22}}`)
	doc, ok := reconstructUsageJSON(frag)
	if !ok {
		t.Fatal("expected to reconstruct usage from a truncated fragment")
	}
	var env struct {
		Usage struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(doc, &env); err != nil {
		t.Fatalf("reconstructed doc is not valid JSON: %v (%s)", err, doc)
	}
	if env.Usage.In != 11 || env.Usage.Out != 22 {
		t.Fatalf("reconstructed usage = %d/%d, want 11/22", env.Usage.In, env.Usage.Out)
	}
}

// TestReconstructUsageJSON_NoUsageReturnsFalse ensures a fragment without any
// usage signal is reported as such so the caller skips attribution rather than
// feeding the meter garbage.
func TestReconstructUsageJSON_NoUsageReturnsFalse(t *testing.T) {
	if _, ok := reconstructUsageJSON([]byte(`...no usable signal here...`)); ok {
		t.Fatal("expected ok=false when no usage object is present")
	}
}

var _ providers.Meter = (*usageMeter)(nil)
