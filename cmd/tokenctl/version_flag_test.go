package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestVersionFlag is the m4 release-hardening regression: `tokenctl --version`
// prints the bare release tag (`tokenctl v0.10.0`) so release-pinning scripts
// can grep a single line, and the `tokenctl version` subcommand surfaces the
// same tag with the richer commit / OS / toolchain detail. Both must agree on
// the tag.
func TestVersionFlag(t *testing.T) {
	t.Run("--version flag", func(t *testing.T) {
		root := newRootCmd()
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetArgs([]string{"--version"})

		if err := root.Execute(); err != nil {
			t.Fatalf("Execute(--version): %v", err)
		}
		got := out.String()
		want := "tokenctl v0.10.0\n"
		if got != want {
			t.Fatalf("--version output = %q, want %q (also stderr=%q)", got, want, errOut.String())
		}
	})

	t.Run("version subcommand", func(t *testing.T) {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"version"})

		if err := root.Execute(); err != nil {
			t.Fatalf("Execute(version): %v", err)
		}
		got := out.String()
		if !strings.HasPrefix(got, "tokenctl v0.10.0") {
			t.Fatalf("version subcommand output = %q, want prefix %q", got, "tokenctl v0.10.0")
		}
		// The subcommand carries the toolchain detail the bare --version flag omits.
		if !strings.Contains(got, "commit") {
			t.Fatalf("version subcommand output = %q, want it to carry commit/OS/toolchain detail", got)
		}
	})
}

// TestVersionConstantPinsReleaseTag guards the shipped release tag the VERSION
// file and publish step depend on. Bumping it without a release is a release-
// engineering error this test catches at CI time.
func TestVersionConstantPinsReleaseTag(t *testing.T) {
	if Version != "v0.10.0" {
		t.Fatalf("main.Version = %q, want %q (bump in lock-step with VERSION)", Version, "v0.10.0")
	}
}

// Compile-time guard that newRootCmd satisfies the cobra.Command contract so a
// future signature change to the builder surfaces here, not at runtime.
var _ = func() *cobra.Command { return newRootCmd() }
