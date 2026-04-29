package httpcache

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak. httpcache is the body-cache layer
// - Upsert + Lookup are synchronous PG calls, no goroutines. The
// tripwire is here so a future fetch-pool / async-fan-out can't silently
// regress.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
