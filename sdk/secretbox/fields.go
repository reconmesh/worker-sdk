package secretbox

import "strings"

// DecryptFields walks cfg in-place and decrypts each string value
// whose dotted path matches an entry in secretFields. Non-encrypted
// strings (legacy plaintext from before I22, or operator-cleared)
// pass through. Returns the count of fields actually decrypted +
// a slice of paths that failed to decrypt (wrong key / corrupt) so
// the caller can log "secret X unavailable" without dropping the
// rest of the config.
//
// Worker semantics: a failed decrypt leaves the original
// "enc:v1:..." string in cfg. The downstream Configurable.Reload
// sees ciphertext for that field; the worker's HTTP call with a
// garbage credential then fails loudly. That's intentional -
// silently dropping the field would mean the worker runs WITHOUT
// authentication when the key is wrong, which is worse than a
// noisy auth failure.
func DecryptFields(cfg map[string]any, secretFields []string, k Key) (decrypted int, failed []string) {
	if cfg == nil || len(secretFields) == 0 {
		return 0, nil
	}
	for _, path := range secretFields {
		segments := splitPath(path)
		if len(segments) == 0 {
			continue
		}
		got, ok := walkValue(cfg, segments)
		if !ok {
			continue
		}
		s, ok := got.(string)
		if !ok || s == "" {
			continue
		}
		if !IsEncrypted(s) {
			continue
		}
		pt, err := Decrypt(k, s)
		if err != nil {
			failed = append(failed, path)
			continue
		}
		setValue(cfg, segments, string(pt))
		decrypted++
	}
	return decrypted, failed
}

// splitPath turns "providers.shodan_key" into ["providers",
// "shodan_key"]. Edge cases (leading dot, double dot, trailing
// dot) are tolerated - operator typos drop the empty segments.
func splitPath(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	parts := strings.Split(path, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// walkValue descends segments into m. Returns (value, true) when
// the path resolves; (nil, false) on missing segment or non-map
// intermediate.
func walkValue(m map[string]any, segments []string) (any, bool) {
	cur := any(m)
	for _, seg := range segments {
		mp, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := mp[seg]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// setValue descends segments and sets the leaf to v. No-op when an
// intermediate segment doesn't exist or isn't a map - we don't
// auto-create the path because that would let a typo'd secret
// field grow the operator's config silently.
func setValue(m map[string]any, segments []string, v any) {
	if len(segments) == 0 {
		return
	}
	cur := m
	for i := 0; i < len(segments)-1; i++ {
		nested, ok := cur[segments[i]].(map[string]any)
		if !ok {
			return
		}
		cur = nested
	}
	last := segments[len(segments)-1]
	if _, present := cur[last]; !present {
		return
	}
	cur[last] = v
}
