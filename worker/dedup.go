package worker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"sort"
)

// DedupHash computes the canonical hash used to deduplicate findings
// across runs. The same logical finding emitted twice - even with
// re-ordered map keys, different float representations, or unrelated
// extra fields the worker added later - produces the same hash.
//
// Hash inputs (ordered):
//   1. f.Kind     (string)
//   2. f.Severity (string)
//   3. f.Data canonical JSON encoding
//
// Title is intentionally NOT in the hash. Rewording a title shouldn't
// fork findings; the latest title wins on update.
//
// Implementation note: we go through encoding/json on a sorted map
// rather than reflection-based hashing because Go's encoding/json
// produces a deterministic byte sequence given sorted keys, and
// canonicalizing JSON is the well-trodden path for "stable hash of a
// JSON document". The cost is one allocation per finding - fine on a
// hot path that's already doing PG inserts.
func DedupHash(f Finding) []byte {
	h := sha256.New()
	// length-prefix each field so "kindA" + "" can't collide with
	// "kind" + "A". Cheap framing; matters in adversarial cases more
	// than in practice but it's free.
	writeLP(h, []byte(f.Kind))
	writeLP(h, []byte(string(f.Severity)))
	canon := canonicalJSON(f.Data)
	writeLP(h, canon)
	return h.Sum(nil)
}

func writeLP(w interface{ Write([]byte) (int, error) }, b []byte) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	_, _ = w.Write(hdr[:])
	_, _ = w.Write(b)
}

// canonicalJSON produces a stable JSON encoding: maps are sorted by
// key recursively, slices keep their order. Returns "null" for nil
// inputs so the hash is well-defined even on findings that omit Data.
func canonicalJSON(v map[string]any) []byte {
	if v == nil {
		return []byte("null")
	}
	out, err := json.Marshal(canonicalize(v))
	if err != nil {
		// Marshal of a sorted map[string]any can't fail in practice;
		// if it does, we surface a stable sentinel rather than panic
		// - the SDK's caller is in a hot path.
		return []byte(`{"__canonical_error__":"true"}`)
	}
	return out
}

// canonicalize walks v and replaces every map[string]any with a
// sortedMap. Slices are walked recursively. Other types pass through.
func canonicalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Use a slice of KV pairs since map iteration order is random;
		// json.Marshal of a map is sorted by Go's encoder anyway, but
		// we want recursive canonicalization on the values.
		ordered := make(orderedMap, 0, len(keys))
		for _, k := range keys {
			ordered = append(ordered, kv{Key: k, Val: canonicalize(t[k])})
		}
		return ordered
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = canonicalize(e)
		}
		return out
	default:
		return t
	}
}

type kv struct {
	Key string `json:"k"`
	Val any    `json:"v"`
}

// orderedMap marshals as a JSON object with sorted keys. Used by
// canonicalize.
type orderedMap []kv

func (o orderedMap) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	for i, e := range o {
		if i > 0 {
			buf = append(buf, ',')
		}
		k, _ := json.Marshal(e.Key)
		buf = append(buf, k...)
		buf = append(buf, ':')
		v, err := json.Marshal(e.Val)
		if err != nil {
			return nil, err
		}
		buf = append(buf, v...)
	}
	return append(buf, '}'), nil
}
