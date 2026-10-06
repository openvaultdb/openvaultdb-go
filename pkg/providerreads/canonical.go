// Package providerreads implements the closed ovdb-provider-read/1 evidence
// contract. Evidence records consumed transient bytes, never bodies or rows.
package providerreads

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Canonical returns RFC 8785 JSON, with ECMAScript numbers and UTF-16 key order.
// No Unicode normalization or recursive field removal is performed.
func Canonical(value any) ([]byte, error) {
	if err := validStrings(reflect.ValueOf(value)); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var parsed any
	if err = d.Decode(&parsed); err != nil {
		return nil, err
	}
	return canonical(parsed)
}
func Digest(value any) (string, error) {
	b, err := Canonical(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}
func validStrings(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			return validStrings(v.Elem())
		}
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid Unicode scalar")
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if err := validStrings(key); err != nil {
				return err
			}
			if err := validStrings(v.MapIndex(key)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := validStrings(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath == "" {
				if err := validStrings(v.Field(i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func canonical(value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		return []byte(strconv.FormatBool(v)), nil
	case string:
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range v {
			switch r {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if r < 0x20 {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte('"')
		return []byte(b.String()), nil
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("nonfinite JSON number")
		}
		if f == 0 {
			return []byte("0"), nil
		}
		format := byte('e')
		if math.Abs(f) >= 1e-6 && math.Abs(f) < 1e21 {
			format = 'f'
		}
		s := strconv.FormatFloat(f, format, -1, 64)
		if format == 'e' {
			parts := strings.Split(s, "e")
			exp, _ := strconv.Atoi(parts[1])
			sign := ""
			if exp >= 0 {
				sign = "+"
			}
			s = parts[0] + "e" + sign + strconv.Itoa(exp)
		}
		return []byte(s), nil
	case []any:
		out := []byte{'['}
		for i, item := range v {
			if i > 0 {
				out = append(out, ',')
			}
			b, err := canonical(item)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
		return append(out, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))
			for k := 0; k < len(a) && k < len(b); k++ {
				if a[k] != b[k] {
					return a[k] < b[k]
				}
			}
			return len(a) < len(b)
		})
		out := []byte{'{'}
		for i, key := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			k, _ := canonical(key)
			out = append(out, k...)
			out = append(out, ':')
			b, err := canonical(v[key])
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
		return append(out, '}'), nil
	}
	return nil, fmt.Errorf("unsupported JSON value")
}
