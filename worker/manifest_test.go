package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name     string
		manifest Manifest
		wantErr  string // substring; empty = expect ok
	}{
		{
			name: "ok minimal",
			manifest: Manifest{
				Tool: "tm-demo", Version: "0.1.0",
				Phases: []Phase{{Name: "demo-phase",
					Consumes: ConsumeSpec{Kinds: []string{"url"}}}},
			},
		},
		{
			name: "tool name with space",
			manifest: Manifest{
				Tool: "tm demo", Version: "0.1.0",
				Phases: []Phase{{Name: "p", Consumes: ConsumeSpec{Kinds: []string{"url"}}}},
			},
			wantErr: "tool",
		},
		{
			name: "missing version",
			manifest: Manifest{
				Tool: "tm-demo",
				Phases: []Phase{{Name: "p", Consumes: ConsumeSpec{Kinds: []string{"url"}}}},
			},
			wantErr: "version",
		},
		{
			name: "no phases",
			manifest: Manifest{
				Tool: "tm-demo", Version: "0.1.0",
			},
			wantErr: "phases",
		},
		{
			name: "duplicate phase",
			manifest: Manifest{
				Tool: "tm-demo", Version: "0.1.0",
				Phases: []Phase{
					{Name: "p", Consumes: ConsumeSpec{Kinds: []string{"url"}}},
					{Name: "p", Consumes: ConsumeSpec{Kinds: []string{"url"}}},
				},
			},
			wantErr: "duplicated",
		},
		{
			name: "missing consumes kind",
			manifest: Manifest{
				Tool: "tm-demo", Version: "0.1.0",
				Phases: []Phase{{Name: "p", Consumes: ConsumeSpec{}}},
			},
			wantErr: "consumes.kinds",
		},
		{
			name: "invalid priority",
			manifest: Manifest{
				Tool: "tm-demo", Version: "0.1.0",
				Phases: []Phase{{Name: "p", PriorityHint: 5,
					Consumes: ConsumeSpec{Kinds: []string{"url"}}}},
			},
			wantErr: "priority_hint",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.manifest.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err %q doesn't contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadManifest_FileNotFound(t *testing.T) {
	if _, err := LoadManifest(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error reading nonexistent manifest")
	}
}

// Pin: description survives YAML round-trip. The Plugins page renders
// it verbatim, so a silent yaml-tag drift would blank every catalog
// card without a build break.
func TestLoadManifest_DescriptionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	yaml := []byte(`tool: tm-demo
version: 0.1.0
maintainer: team-tools
description: "demo worker · short summary."
phases:
  - name: demo-phase
    consumes:
      kinds: [url]
`)
	if err := os.WriteFile(path, yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := "demo worker · short summary."
	if m.Description != want {
		t.Fatalf("description = %q, want %q", m.Description, want)
	}
}
