package dns

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak (Stage A5). The dns package owns the LRU
// cache + the tm-resolve dial-and-stream path; both are
// goroutine-light today (no background loops in this package — the
// dns-service binary owns those) but the tripwire catches a future
// spawn here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
