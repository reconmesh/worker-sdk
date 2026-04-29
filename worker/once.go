package worker

import (
	"encoding/json"
	"fmt"
	"os"
)

// parseOnceAsset accepts the -asset flag's JSON payload. We're
// deliberately permissive - operators in a hurry might pass just
// `{"value":"https://x.com"}` and expect a sane default kind.
//
// The function returns errors as plain go errors so Serve's `die` can
// format them; no panics on bad input.
func parseOnceAsset(raw string) (Asset, error) {
	var a Asset
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return Asset{}, fmt.Errorf("parse: %w", err)
	}
	if a.Kind == "" {
		// Best effort: assume the operator means a URL when none is given.
		// Workers that consume something else will reject the job in Run.
		a.Kind = "url"
	}
	if a.Value == "" {
		return Asset{}, fmt.Errorf("asset.value required")
	}
	return a, nil
}

// printOnceResult writes Result to stdout as pretty JSON. -once is a
// debug aid; readability beats throughput.
func printOnceResult(r Result) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}
