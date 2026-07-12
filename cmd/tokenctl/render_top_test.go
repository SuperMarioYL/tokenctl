package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRenderTop_ShowsPreemptCount is the regression for
// fix-tokenctl-top-drops-preempt-count. The proxy's Tree.Snapshot() emits
// `preempts_total` (and the Prometheus collector exposes it), but the CLI-side
// topSnapshot struct had no matching field and renderTop's header printed only
// throttles + denies — so m3 arbiter preemptions (the plan's headline, shown in
// the demo asciinema) were invisible in the `tokenctl top` live view.
//
// This test decodes a snapshot JSON carrying preempts_total (proving the wire
// tag round-trips into the CLI struct) and asserts renderTop's header surfaces
// the preempt count. Against the pre-fix code it FAILS twice: the json field is
// dropped on decode (snap.Preempts stays 0) AND the header format has no
// preempts token at all.
func TestRenderTop_ShowsPreemptCount(t *testing.T) {
	// A realistic /v1/snapshot body: the server side (internal/budget) serialises
	// preempts_total alongside throttles_total / denies_total.
	raw := []byte(`{
		"generated_at": "2026-06-05T03:42:11Z",
		"groups": [
			{"path": "acme.team-platform.alice", "weight": 50, "window": "24h",
			 "budget_tokens": 5000000, "consumed_tokens": 4200, "in_flight": 1,
			 "state": "ok", "frac_used": 0.0}
		],
		"in_flight": 1,
		"denies_total": 2,
		"throttles_total": 5,
		"preempts_total": 3
	}`)

	var snap topSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	// The preempts_total field must land in the CLI struct — the pre-fix struct
	// had no such field, so this stayed 0 and the count was silently lost.
	if snap.Preempts != 3 {
		t.Fatalf("snap.Preempts = %d, want 3 (preempts_total dropped on decode)", snap.Preempts)
	}

	var buf bytes.Buffer
	renderTop(&buf, &snap)
	out := buf.String()

	if !strings.Contains(out, "preempts=3") {
		t.Errorf("renderTop header missing preempt count; want a %q token, got:\n%s", "preempts=3", out)
	}
	// The existing signals must remain present so we didn't regress the header.
	for _, want := range []string{"throttles=5", "denies=2", "in-flight=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTop header missing %q, got:\n%s", want, out)
		}
	}
}
