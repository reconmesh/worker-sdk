package worker

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Manifest is the YAML contract every worker publishes alongside its
// binary. The control plane reads it on worker registration to know
// which jobs to dispatch; the SDK validates it at boot so a misconfigured
// manifest fails the worker process before it serves a single job.
//
// The on-disk shape mirrors this struct 1:1. We use yaml v3 tags rather
// than json so manifests can carry comments — they're operator-facing,
// not just machine-read.
type Manifest struct {
	// Tool is the canonical worker name. Must match the value
	// returned by Tool.Name(); the SDK enforces this on boot.
	Tool string `yaml:"tool"`
	// Version is the worker's own semver. Reported in the workers
	// table and in metrics labels.
	Version string `yaml:"version"`
	// Maintainer is a free-form team or individual identifier surfaced
	// in the UI. Optional.
	Maintainer string `yaml:"maintainer,omitempty"`
	// Phases declares one or more pipeline phases this worker handles.
	// A single binary can serve several phases by branching in Run on
	// Job.Phase; this is rare in practice.
	Phases []Phase `yaml:"phases"`
}

// Phase describes one logical step in a scope's pipeline.
type Phase struct {
	// Name is the phase identifier referenced in Scope.Phases[].
	// Convention: kebab-case, prefixed by the tool when ambiguous
	// (e.g. "techmapper-fingerprint" if the phase name alone clashes
	// across tools).
	Name string `yaml:"name"`
	// Consumes declares which assets feed this phase.
	Consumes ConsumeSpec `yaml:"consumes"`
	// Produces is documentary — it doesn't restrict what Run can
	// emit, but the control plane's "expected outputs" UI uses it
	// to show operators what a phase is meant to do.
	Produces ProduceSpec `yaml:"produces"`
	// ConcurrencyPerHost caps how many simultaneous Run invocations
	// can target the same host. 0 means unlimited (default for tools
	// that don't hit a network endpoint).
	ConcurrencyPerHost int `yaml:"concurrency_per_host,omitempty"`
	// AvgDurationMS is a hint used by the control plane's capacity
	// planner to size queues and warn on backlogs. It's a hint only —
	// not enforced.
	AvgDurationMS int `yaml:"avg_duration_ms,omitempty"`
	// TimeoutSeconds caps a single Run invocation. 0 means use the
	// SDK default (300s). Per-job override is possible via
	// Job.Deadline.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// PriorityHint biases this phase's jobs in the queue. 1 (highest)
	// to 4 (lowest, default). Operators can still override on a
	// per-run basis.
	PriorityHint int `yaml:"priority_hint,omitempty"`
}

// ConsumeSpec describes the asset selector for a phase.
type ConsumeSpec struct {
	// Kinds is the disjunction of asset kinds the phase accepts.
	// Empty means the phase doesn't run at the asset level (e.g.
	// scheduled cron tasks); rare.
	Kinds []string `yaml:"kinds"`
	// Filter is a server-side predicate over asset.attrs JSONB. It
	// uses the limited expression grammar in `worker/filter`. Empty
	// = match every asset of the listed kinds.
	Filter string `yaml:"filter,omitempty"`
}

// ProduceSpec is documentary metadata about a phase's outputs.
type ProduceSpec struct {
	// Assets lists the kinds this phase typically emits. Empty list
	// means the phase only emits findings.
	Assets []string `yaml:"assets,omitempty"`
	// FindingKinds lists the finding kinds this phase emits. Used
	// by the UI to pre-populate filter dropdowns even before any
	// finding has been observed.
	FindingKinds []string `yaml:"finding_kinds,omitempty"`
}

// LoadManifest reads + validates a manifest from path. The standard
// boot path is LoadManifest("manifest.yaml") next to the binary; the
// SDK's Serve() does this for you.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest read: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("manifest validate: %w", err)
	}
	return &m, nil
}

// nameRe matches the snake_case-or-kebab-case identifiers we accept
// for tool, phase, and kind names. Conservative on purpose — too lax
// and we end up with `My Tool 2!` showing up in metrics labels.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)

// Validate runs basic structural checks. It's not exhaustive — the
// filter grammar parser does deeper validation when filters are
// loaded — but catches the common boot-time mistakes (typo'd phase
// name, missing version, conflicting priorities).
func (m *Manifest) Validate() error {
	if !nameRe.MatchString(m.Tool) {
		return fmt.Errorf("tool: %q is not a valid identifier", m.Tool)
	}
	if m.Version == "" {
		return errors.New("version: required")
	}
	if len(m.Phases) == 0 {
		return errors.New("phases: at least one required")
	}
	seen := map[string]bool{}
	for i, p := range m.Phases {
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("phases[%d].name %q invalid", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("phases[%d].name %q duplicated", i, p.Name)
		}
		seen[p.Name] = true
		if len(p.Consumes.Kinds) == 0 {
			return fmt.Errorf("phases[%d] (%s): consumes.kinds required", i, p.Name)
		}
		for j, k := range p.Consumes.Kinds {
			if !nameRe.MatchString(k) {
				return fmt.Errorf("phases[%d].consumes.kinds[%d] %q invalid", i, j, k)
			}
		}
		if p.PriorityHint < 0 || p.PriorityHint > 4 {
			return fmt.Errorf("phases[%d].priority_hint must be 0..4", i)
		}
		if p.TimeoutSeconds < 0 {
			return fmt.Errorf("phases[%d].timeout_seconds negative", i)
		}
	}
	return nil
}
