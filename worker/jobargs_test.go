package worker

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// Pin: JobKind is the wire contract between cascade (producer in
// controlplane) and worker-sdk runtime (consumer here). A silent
// rename on either side would make every worker stop matching jobs
// without a build break · River pre-registers by Kind() string.
//
// controlplane/internal/jobtype/jobtype_test.go pins the same value
// from the producer side.
func TestJobKindIsStable(t *testing.T) {
	const want = "reconmesh.cascade.v1"
	if JobKind != want {
		t.Fatalf("JobKind drift · got %q, want %q · controlplane jobtype.JobKind pins the same value", JobKind, want)
	}
	if (CascadeArgs{}).Kind() != want {
		t.Fatalf("CascadeArgs.Kind() must equal JobKind, got %q", (CascadeArgs{}).Kind())
	}
}

// Pin: ForceFresh round-trips through JSON when set. Manual-rescan
// endpoints depend on this field reaching the worker; a silent type
// change (bool → string) would silently disable the bypass.
func TestCascadeArgsForceFreshRoundTrip(t *testing.T) {
	args := CascadeArgs{
		ScopeID:    uuid.New(),
		AssetID:    uuid.New(),
		Phase:      "http-probe",
		ForceFresh: true,
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var back CascadeArgs
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.ForceFresh {
		t.Fatalf("ForceFresh lost in round-trip: %s", string(raw))
	}
}
