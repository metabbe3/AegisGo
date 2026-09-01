package mcpclient

import (
	"context"
	"testing"
)

func TestConnectNoSpecs(t *testing.T) {
	tools, release, err := Connect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if tools != nil {
		t.Errorf("tools = %v, want nil", tools)
	}
}

func TestDialSpecErrors(t *testing.T) {
	cases := []string{
		"stdio:",            // empty command
		"ftp://example.com", // unsupported scheme
		"nonsense",          // neither stdio: nor http(s)://
	}
	for _, spec := range cases {
		if _, err := dial(context.Background(), spec); err == nil {
			t.Errorf("dial(%q) = nil error, want failure", spec)
		}
	}
}
