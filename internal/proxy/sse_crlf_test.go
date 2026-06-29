package proxy

import (
	"bytes"
	"io"
	"testing"
)

// eventRecordingMeter records every (event, data) pair Observe is called with,
// so a test can assert that the SSE reader framed the stream into the right
// number of discrete events with the right names — the property that breaks
// when events are not split incrementally.
type eventRecordingMeter struct {
	events []recordedEvent
}

type recordedEvent struct {
	name string
	data string
}

func (m *eventRecordingMeter) Observe(event string, data []byte) (int64, int64) {
	m.events = append(m.events, recordedEvent{name: event, data: string(data)})
	// Treat each event's data as carrying one output token so the admission
	// also sees a per-event credit; the exact value is irrelevant to framing.
	return 0, 1
}

// claudeStyleSSE returns a two-event Anthropic-shaped SSE body (message_start
// then message_delta) using the supplied line/record separators, so the same
// payload can be emitted with LF ("\n") or CRLF ("\r\n") framing.
func claudeStyleSSE(eol string) []byte {
	var b bytes.Buffer
	// event 1
	b.WriteString("event: message_start" + eol)
	b.WriteString(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}` + eol)
	b.WriteString(eol) // blank line terminates the event
	// event 2
	b.WriteString("event: message_delta" + eol)
	b.WriteString(`data: {"type":"message_delta","usage":{"input_tokens":10,"output_tokens":25}}` + eol)
	b.WriteString(eol)
	return b.Bytes()
}

// TestSSEMeteredReader_LFFraming is the baseline: an LF-framed stream splits
// into exactly two named events.
func TestSSEMeteredReader_LFFraming(t *testing.T) {
	body := claudeStyleSSE("\n")
	adm := newFakeAdmission()
	m := newMetrics()
	meter := &eventRecordingMeter{}
	rc := newSSEMeteredReader(io.NopCloser(bytes.NewReader(body)), meter, adm, m, "claude")

	got := drainReader(t, rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("client bytes altered under LF framing")
	}
	assertTwoClaudeEvents(t, meter)
}

// TestSSEMeteredReader_CRLFFraming is the regression for
// fix-sse-crlf-event-splitting: a CRLF-framed stream (\r\n\r\n between events)
// MUST split into the same two discrete named events. Before the fix, the
// reader looked only for "\n\n" — which never occurs in a "\r\n\r\n"-framed
// body — so every event piled up until EOF and collapsed into one pseudo-event
// carrying only the last "event:" name, dropping message_start's usage.
func TestSSEMeteredReader_CRLFFraming(t *testing.T) {
	body := claudeStyleSSE("\r\n")
	adm := newFakeAdmission()
	m := newMetrics()
	meter := &eventRecordingMeter{}
	rc := newSSEMeteredReader(io.NopCloser(bytes.NewReader(body)), meter, adm, m, "claude")

	got := drainReader(t, rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("client bytes altered under CRLF framing")
	}
	assertTwoClaudeEvents(t, meter)
}

// TestSSEMeteredReader_CRLFFramingChunkedReads exercises the incremental path:
// the same CRLF body fed in small chunks (so an event boundary straddles a Read)
// must still frame into two events, proving the boundary search works across
// the pending buffer, not just on a single whole-body Read.
func TestSSEMeteredReader_CRLFFramingChunkedReads(t *testing.T) {
	body := claudeStyleSSE("\r\n")
	adm := newFakeAdmission()
	m := newMetrics()
	meter := &eventRecordingMeter{}
	// chunkReader hands out the body 7 bytes at a time so boundaries land
	// mid-chunk repeatedly.
	rc := newSSEMeteredReader(&chunkReader{data: body, step: 7}, meter, adm, m, "claude")

	_ = drainReader(t, rc)
	assertTwoClaudeEvents(t, meter)
}

func assertTwoClaudeEvents(t *testing.T, meter *eventRecordingMeter) {
	t.Helper()
	if len(meter.events) != 2 {
		t.Fatalf("expected 2 framed SSE events, got %d: %+v", len(meter.events), meter.events)
	}
	if meter.events[0].name != "message_start" {
		t.Fatalf("event[0] name = %q, want message_start", meter.events[0].name)
	}
	if meter.events[1].name != "message_delta" {
		t.Fatalf("event[1] name = %q, want message_delta", meter.events[1].name)
	}
}

// chunkReader yields data in fixed-size steps to exercise the multi-Read path.
type chunkReader struct {
	data []byte
	step int
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + c.step
	if end > len(c.data) {
		end = len(c.data)
	}
	n := copy(p, c.data[c.pos:end])
	c.pos += n
	if c.pos >= len(c.data) {
		return n, io.EOF
	}
	return n, nil
}

func (c *chunkReader) Close() error { return nil }
