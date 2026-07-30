package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Key derives a stable cache key from the resolved model id, the request path,
// and the request body. The body is canonicalized — parsed and re-marshaled so
// map key order and insignificant whitespace do not matter — before hashing, so
// two semantically identical requests collide. If the body is not valid JSON the
// raw bytes are hashed as-is (such a request is not normally cacheable anyway).
func Key(model, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	if canon, ok := canonicalJSON(body); ok {
		h.Write(canon)
	} else {
		h.Write(body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON round-trips body through a generic decode/encode so object keys
// are sorted (Go marshals map[string]any keys in sorted order) and formatting is
// normalized. Returns ok=false if body is not valid JSON.
func canonicalJSON(body []byte) ([]byte, bool) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}
