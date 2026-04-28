package mtls

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak (Stage A5). mtls builds TLS configs and
// http.Transports — no goroutines today, but a future client-pooling
// addition could leak; the tripwire catches that.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
