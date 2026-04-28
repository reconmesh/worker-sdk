package secretbox

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak (Stage A5). secretbox is pure-crypto with no
// goroutines today; the guard is here as a tripwire for the future.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
