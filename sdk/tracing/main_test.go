package tracing

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak (Stage A5). tracing wraps OTel SDK setup;
// real exporter goroutines spawn at Init time but the test surface
// is span-shape assertions that should not. Tripwire catches a spawn
// that escapes without shutdown.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
