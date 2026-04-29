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
// than json so manifests can carry comments - they're operator-facing,
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
	// Description is a one-line operator-facing summary of what the
	// worker does. Surfaced verbatim on the Plugins page so an operator
	// scanning the catalog can answer "what does tm-vulnx do?" without
	// drilling into the manifest. Keep it short (one sentence, ~80
	// chars) so it fits the card layout.
	Description string `yaml:"description,omitempty"`
	// Config is the static defaults the tool ships with. Operator
	// overrides land in PG (tool_configs) and the SDK runtime
	// deep-merges override over Config before invoking
	// Configurable.ReloadConfig. Free-form on purpose - each tool
	// owns its own config schema. Surfaced in /api/plugins so the
	// UI can show defaults vs override.
	Config map[string]any `yaml:"config,omitempty"`
	// Secrets is the dotted-path list of config fields encrypted at
	// rest. The controlplane API masks these as "***"
	// in GET responses and encrypts via AES-256-GCM on PUT. The SDK
	// runtime decrypts them on the worker side before passing the
	// merged config to ReloadConfig - workers see plaintext API
	// keys at scan time without ever round-tripping the plaintext
	// through the operator UI again.
	//
	// Format: ["api_key", "providers.shodan_key"] - top-level keys
	// or dotted paths (matches the secretbox field walker).
	//
	// Required for any encrypted field. A field that's encrypted in
	// PG but missing from this list arrives at ReloadConfig as
	// "enc:v1:..." ciphertext and the worker's HTTP calls fail
	// with garbage credentials - the manifest is the source of
	// truth on what's a secret.
	Secrets []string `yaml:"secrets,omitempty"`
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
	// Produces is documentary - it doesn't restrict what Run can
	// emit, but the control plane's "expected outputs" UI uses it
	// to show operators what a phase is meant to do.
	Produces ProduceSpec `yaml:"produces"`
	// ConcurrencyPerHost caps how many simultaneous Run invocations
	// can target the same host. 0 means unlimited (default for tools
	// that don't hit a network endpoint).
	ConcurrencyPerHost int `yaml:"concurrency_per_host,omitempty"`
	// AvgDurationMS is a hint used by the control plane's capacity
	// planner to size queues and warn on backlogs. It's a hint only -
	// not enforced.
	AvgDurationMS int `yaml:"avg_duration_ms,omitempty"`
	// TimeoutSeconds caps a single Run invocation. 0 means use the
	// SDK default (300s). Per-job override is possible via
	// Job.Deadline.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// PriorityHint biases this phase's jobs in the queue. 1 (highest)
	// through 9 (lowest); 0 means "use the run's default". The
	// cascade engine clamps to River's 1..4 range at queue insertion
	// (River only supports those four), so values 5..9 are documentary
	// intent · they all map to River priority 4. Operators can still
	// override on a per-run basis.
	PriorityHint int `yaml:"priority_hint,omitempty"`
	// UI describes how the web-ui should surface this phase's
	// observations. Two modes:
	//
	//   - declarative: a tab + view spec the front renders generically
	//     (table of attrs, key-value list, markdown). Default - most
	//     plugins need nothing more.
	//   - federated:  a Module Federation remote entry URL the front
	//     loads at runtime. The remote can render arbitrary React
	//     components and inject into any of the documented UI slots
	//     (dashboard, scope-list, scope-overview, host-page).
	//
	// Both modes can coexist: a plugin may declare a fallback table
	// AND a federated component, and the front uses the federated
	// one when reachable, the declarative one otherwise.
	UI *UISpec `yaml:"ui,omitempty"`
}

// UISpec is the contract between a worker and the web-ui. The host
// reads a connected worker's manifest, builds the navigation, and
// either renders the declarative views or loads the federated module.
//
// Stability: every field here is part of the wire contract. Adding
// is MINOR, removing is MAJOR (worker authors who depended on a
// removed field have to update). Renaming a render kind is MAJOR.
type UISpec struct {
	// Tab is shown on the host page (and elsewhere depending on
	// where the plugin's data lives). Required when at least one
	// view is declared.
	Tab *UITab `yaml:"tab,omitempty"`
	// Views are rendered inside the tab. The front composes them
	// vertically. Each view picks one of the supported render kinds
	// (table, kv, markdown, tree, link-list, json).
	Views []UIView `yaml:"views,omitempty"`
	// Federated, when non-empty, points to a Module Federation
	// remote entry the front loads at runtime. The remote MAY
	// expose components for any of these slots:
	//
	//   "host-tab"        rendered inside the tab on the host page
	//   "host-overview"   inserted into the host page's overview block
	//   "scope-overview"  inserted into the scope dashboard
	//   "scope-list"      adds a column / badge to the scopes list
	//   "global-news"     inserts cards into the cross-scope News feed
	//
	// The host validates the slot names; unknown slots are ignored
	// rather than rejected so a newer worker doesn't break older
	// fronts.
	Federated *UIFederated `yaml:"federated,omitempty"`
}

// UITab is the navigation entry. The front turns this into a button
// in the host-page tab strip with an optional badge.
type UITab struct {
	Label string `yaml:"label"`
	// Lucide / Heroicons name. Front maps to its icon set.
	Icon string `yaml:"icon,omitempty"`
	// Badge controls the small bubble on the tab. Supported:
	//   "count"      number of items in the first table view
	//   "exists"     ✓ / ✗ on a bool key (configured per view)
	//   ""           no badge
	Badge string `yaml:"badge,omitempty"`
	// AttrsPath: dotted path under assets.attrs where this plugin's
	// data lives. The front reads the asset row, drills here, and
	// passes the slice to the view renderers. Required for
	// declarative views.
	AttrsPath string `yaml:"attrs_path,omitempty"`
}

// UIView is one rendered block inside a tab. Kind picks the renderer;
// the rest of the fields are kind-specific. We don't union-type these
// in YAML to keep the manifest readable; unused fields are ignored.
type UIView struct {
	Kind string `yaml:"kind"` // table | kv | markdown | tree | link-list | json
	// Title shown above the block. Optional.
	Title string `yaml:"title,omitempty"`

	// table-specific:
	Columns []UIColumn `yaml:"columns,omitempty"`
	Sort    []string   `yaml:"sort,omitempty"`
	Filters []string   `yaml:"filters,omitempty"`

	// kv-specific:
	Keys []UIKVKey `yaml:"keys,omitempty"`

	// markdown-specific:
	// the content lives at the asset's attrs.<attrs_path>.<source>
	// (assumed to be a markdown string). Empty source = use the
	// whole node as a string.
	Source string `yaml:"source,omitempty"`
}

// UIColumn describes one column in a table view.
type UIColumn struct {
	Field     string `yaml:"field"`
	Label     string `yaml:"label,omitempty"`
	Align     string `yaml:"align,omitempty"`     // left|right|center
	Monospace bool   `yaml:"monospace,omitempty"`
	Mask      bool   `yaml:"mask,omitempty"`      // for secrets: render obscured by default
	Link      bool   `yaml:"link,omitempty"`      // value rendered as a clickable link
}

// UIKVKey describes one row in a key-value view.
type UIKVKey struct {
	Field string `yaml:"field"`
	Label string `yaml:"label,omitempty"`
	Hint  string `yaml:"hint,omitempty"` // tooltip
}

// UIFederated points the host at a Module Federation remote.
type UIFederated struct {
	// RemoteEntry is the URL the host imports. Typically served by
	// the worker's own admin port, or by a static asset host.
	RemoteEntry string `yaml:"remote_entry"`
	// Scope is the MF "scope" name (the identifier used in Vite /
	// Webpack federation config). Conventionally matches the tool
	// name with dashes replaced by underscores.
	Scope string `yaml:"scope"`
	// Slots maps reconmesh slot names to the modules the remote
	// exposes. Example: { "host-tab": "./HostTab" }.
	Slots map[string]string `yaml:"slots"`
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
// for tool, phase, and kind names. Conservative on purpose - too lax
// and we end up with `My Tool 2!` showing up in metrics labels.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)

// Validate runs basic structural checks. It's not exhaustive - the
// filter grammar parser does deeper validation when filters are
// loaded - but catches the common boot-time mistakes (typo'd phase
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
		if p.PriorityHint < 0 || p.PriorityHint > 9 {
			return fmt.Errorf("phases[%d].priority_hint must be 0..9", i)
		}
		if p.TimeoutSeconds < 0 {
			return fmt.Errorf("phases[%d].timeout_seconds negative", i)
		}
	}
	return nil
}
