package providerreads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decode reads the closed wire envelope without accepting unknown, duplicate,
// missing, null or invalid-Unicode fields. Validate must follow with trusted Plan.
func Decode(raw []byte) (Envelope, error) {
	if len(raw) > MaxMetadataBytes || !utf8.Valid(raw) {
		return Envelope{}, fmt.Errorf("invalid wire evidence size or Unicode")
	}
	if err := scalarEscapes(raw); err != nil {
		return Envelope{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := readJSON(decoder)
	if err != nil {
		return Envelope{}, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return Envelope{}, fmt.Errorf("extra JSON value")
	}
	if err = closedShape(value, reflect.TypeFor[Envelope]()); err != nil {
		return Envelope{}, err
	}
	var e Envelope
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&e)
	return e, err
}
func readJSON(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		out := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid JSON key")
			}
			if _, ok = out[k]; ok {
				return nil, fmt.Errorf("duplicate JSON key")
			}
			out[k], err = readJSON(d)
			if err != nil {
				return nil, err
			}
		}
		_, err = d.Token()
		return out, err
	case '[':
		out := []any{}
		for d.More() {
			v, err := readJSON(d)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		_, err = d.Token()
		return out, err
	}
	return nil, fmt.Errorf("invalid JSON delimiter")
}
func closedShape(v any, t reflect.Type) error {
	if v == nil {
		return fmt.Errorf("null evidence field")
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("expected evidence object")
		}
		known := map[string]bool{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			parts := strings.Split(f.Tag.Get("json"), ",")
			key := parts[0]
			known[key] = true
			item, present := object[key]
			if !present {
				if len(parts) < 2 || parts[1] != "omitempty" {
					return fmt.Errorf("missing %s", key)
				}
				continue
			}
			if err := closedShape(item, f.Type); err != nil {
				return err
			}
		}
		for key := range object {
			if !known[key] {
				return fmt.Errorf("unknown evidence field %s", key)
			}
		}
	case reflect.Slice:
		array, ok := v.([]any)
		if !ok || len(array) > MaxItems {
			return fmt.Errorf("invalid evidence array")
		}
		for _, item := range array {
			if err := closedShape(item, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.String:
		if value, ok := v.(string); !ok || value == "" {
			return fmt.Errorf("invalid evidence string")
		}
	case reflect.Int, reflect.Int64:
		if _, ok := v.(json.Number); !ok {
			return fmt.Errorf("invalid evidence number")
		}
	}
	return nil
}

// encoding/json substitutes U+FFFD for lone escaped surrogates. Refuse those
// before decoding, while accepting literal U+FFFD and escaped backslashes.
func scalarEscapes(raw []byte) error {
	in := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			in = !in
			continue
		}
		if !in || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return fmt.Errorf("invalid JSON escape")
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return fmt.Errorf("invalid Unicode escape")
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return e
		}
		i += 4
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return fmt.Errorf("invalid Unicode scalar")
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return fmt.Errorf("invalid Unicode scalar")
			}
			i += 6
		} else if n >= 0xdc00 && n <= 0xdfff {
			return fmt.Errorf("invalid Unicode scalar")
		}
	}
	return nil
}
