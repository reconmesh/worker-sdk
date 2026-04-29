package worker

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak so the worker package's tests fail loudly
// on goroutine leaks (Stage A5).
//
// The worker package owns the runtime loop, the dispatcher pool,
// the cascade replay path, and the dedup-bookkeeping ticker - every
// one of those is a goroutine that MUST shut down on ctx cancel.
// Pure-math tests (asset_writer fingerprintAttrs, manifest parse,
// dedup hash) shouldn't spawn goroutines at all; goleak catches a
// regression where someone adds a spawn without a corresponding
// shutdown.
//
// goleak's defaults already filter Go runtime goroutines.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
