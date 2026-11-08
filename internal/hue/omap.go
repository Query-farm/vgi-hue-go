package hue

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// kv is one entry of an ordered JSON object.
type kv struct {
	K string
	V any
}

// omap is a JSON object that keeps insertion order (like a JS object), so
// request bodies serialise exactly as the TypeScript worker's do.
type omap []kv

// set replaces a key in place (keeping its position, as JS does) or appends it.
func (m omap) set(k string, v any) omap {
	for i := range m {
		if m[i].K == k {
			m[i].V = v
			return m
		}
	}
	return append(m, kv{k, v})
}

func (m omap) get(k string) (any, bool) {
	for _, e := range m {
		if e.K == k {
			return e.V, true
		}
	}
	return nil, false
}

func (m omap) has(k string) bool { _, ok := m.get(k); return ok }

func (m omap) without(keys ...string) omap {
	out := omap{}
	for _, e := range m {
		skip := false
		for _, k := range keys {
			skip = skip || e.K == k
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

func (m omap) sub(k string) omap {
	v, _ := m.get(k)
	o, _ := v.(omap)
	return o
}

// MarshalJSON writes the keys in order.
func (m omap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range m {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(e.K)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(e.V)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads an object keeping key order; nested objects become omap.
func (m *omap) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		return err
	}
	o, ok := v.(omap)
	if !ok {
		return fmt.Errorf("not a JSON object")
	}
	*m = o
	return nil
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out := omap{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, kv{kt.(string), val})
			}
			_, err := dec.Token()
			return out, err
		case '[':
			var out []any
			for dec.More() {
				val, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, val)
			}
			_, err := dec.Token()
			return out, err
		}
	case json.Number:
		return t.Float64()
	}
	return tok, nil
}

// num reads a JSON number of any Go numeric type.
func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
