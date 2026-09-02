// Smoke test for the shim: it exists only to call cli.Main with the
// process streams, so proving the version subcommand round-trips through
// this package's import of internal/cli is the whole contract.
package main

import (
	"bytes"
	"context"
	"testing"

	"aegisgo/internal/cli"
)

func TestShimDispatchesToCli(t *testing.T) {
	var out bytes.Buffer
	if code := cli.Main(context.Background(), []string{"version"}, &out, &out); code != 0 {
		t.Fatalf("cli.Main version exit = %d, want 0", code)
	}
	if !bytes.Contains(out.Bytes(), []byte("aegis ")) {
		t.Errorf("output = %q, want the version line", out.String())
	}
}
