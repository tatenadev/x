// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package codec

import (
	"encoding"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// maxFormBody caps a form-encoded response body; such bodies are small
// (token endpoints and the like), and parsing needs the whole body.
const maxFormBody = 1 << 20

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

// Form is the application/x-www-form-urlencoded codec. It encodes
// url.Values, map[string]string, map[string][]string, and structs (or
// pointers to them) whose fields use `form:"name,omitempty"` tags:
//
//   - The name defaults to the Go field name; "-" skips the field.
//   - Supported field types are strings, bools, integers, floats,
//     encoding.TextMarshaler/TextUnmarshaler implementations, pointers to
//     any of those, and slices of them (one value per element). A []byte
//     is a single string.
//   - Nil pointers are always omitted. With omitempty, zero values and
//     empty slices are omitted too.
//   - Embedded structs without a tag are flattened.
//
// Decode fills a *url.Values or a pointer to such a struct.
type Form struct{}

// ContentType returns "application/x-www-form-urlencoded".
func (Form) ContentType() string { return ContentTypeForm }

// Encode writes v to w as a form body. Keys are sorted.
func (Form) Encode(w io.Writer, v any) error {
	vals, err := FormValues(v)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, vals.Encode())
	return err
}

// Decode parses a form body from r into v.
func (Form) Decode(r io.Reader, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxFormBody+1))
	if err != nil {
		return fmt.Errorf("codec: form: read: %w", err)
	}
	if len(b) > maxFormBody {
		return fmt.Errorf("codec: form: body exceeds %d bytes", maxFormBody)
	}
	vals, err := url.ParseQuery(string(b))
	if err != nil {
		return fmt.Errorf("codec: form: %w", err)
	}
	if p, ok := v.(*url.Values); ok {
		*p = vals
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("codec: form: decode into %T: want *url.Values or a pointer to a struct", v)
	}
	return decodeStruct(vals, rv.Elem())
}

// FormValues converts v to url.Values using the rules documented on [Form].
// It is useful for query strings as well as bodies.
func FormValues(v any) (url.Values, error) {
	switch v := v.(type) {
	case url.Values:
		return v.Clone(), nil
	case map[string][]string:
		return url.Values(v).Clone(), nil
	case map[string]string:
		vals := make(url.Values, len(v))
		for k, s := range v {
			vals.Set(k, s)
		}
		return vals, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("codec: form: cannot encode %T", v)
	}
	vals := make(url.Values)
	if err := encodeStruct(vals, rv); err != nil {
		return nil, err
	}
	return vals, nil
}

// formField is one tagged struct field.
type formField struct {
	name      string
	omitempty bool
}

// parseFormTag reports the field's form name and options, and whether the
// field takes part at all. Embedded structs without a tag are flattened by
// the caller.
func parseFormTag(sf reflect.StructField) (formField, bool) {
	if !sf.IsExported() {
		return formField{}, false
	}
	tag, hasTag := sf.Tag.Lookup("form")
	if tag == "-" {
		return formField{}, false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if !hasTag || name == "" {
		name = sf.Name
	}
	return formField{name: name, omitempty: opts == "omitempty"}, true
}

func flatten(sf reflect.StructField) bool {
	_, hasTag := sf.Tag.Lookup("form")
	return sf.Anonymous && !hasTag && sf.Type.Kind() == reflect.Struct
}

func encodeStruct(vals url.Values, rv reflect.Value) error {
	for sf, fv := range rv.Fields() {
		if flatten(sf) {
			if err := encodeStruct(vals, fv); err != nil {
				return err
			}
			continue
		}
		ff, ok := parseFormTag(sf)
		if !ok {
			continue
		}
		strs, err := formatField(fv, ff.omitempty)
		if err != nil {
			return fmt.Errorf("codec: form: field %s: %w", sf.Name, err)
		}
		for _, s := range strs {
			vals.Add(ff.name, s)
		}
	}
	return nil
}

// formatField returns the form values for one field; none means omit.
func formatField(fv reflect.Value, omitempty bool) ([]string, error) {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil, nil
		}
		fv = fv.Elem()
	}
	if omitempty && fv.IsZero() {
		return nil, nil
	}
	if _, isText := textMarshaler(fv); fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() != reflect.Uint8 && !isText {
		strs := make([]string, 0, fv.Len())
		for _, ev := range fv.Seq2() {
			if ev.Kind() == reflect.Pointer {
				if ev.IsNil() {
					continue
				}
				ev = ev.Elem()
			}
			s, err := formatScalar(ev)
			if err != nil {
				return nil, err
			}
			strs = append(strs, s)
		}
		return strs, nil
	}
	s, err := formatScalar(fv)
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}

// textMarshaler returns v as an encoding.TextMarshaler, including through
// a pointer receiver when v is addressable.
func textMarshaler(v reflect.Value) (encoding.TextMarshaler, bool) {
	if v.CanInterface() {
		if m, ok := v.Interface().(encoding.TextMarshaler); ok {
			return m, true
		}
	}
	if v.CanAddr() {
		if m, ok := v.Addr().Interface().(encoding.TextMarshaler); ok {
			return m, true
		}
	}
	return nil, false
}

func formatScalar(v reflect.Value) (string, error) {
	if m, ok := textMarshaler(v); ok {
		b, err := m.MarshalText()
		return string(b), err
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, v.Type().Bits()), nil
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return string(v.Bytes()), nil
		}
	}
	return "", fmt.Errorf("unsupported type %s", v.Type())
}

func decodeStruct(vals url.Values, rv reflect.Value) error {
	for sf, fv := range rv.Fields() {
		if flatten(sf) {
			if err := decodeStruct(vals, fv); err != nil {
				return err
			}
			continue
		}
		ff, ok := parseFormTag(sf)
		if !ok {
			continue
		}
		strs, present := vals[ff.name]
		if !present {
			continue
		}
		if err := setField(fv, strs); err != nil {
			return fmt.Errorf("codec: form: field %s: %w", sf.Name, err)
		}
	}
	return nil
}

func setField(fv reflect.Value, strs []string) error {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		fv = fv.Elem()
	}
	if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() != reflect.Uint8 && !reflect.PointerTo(fv.Type()).Implements(textUnmarshalerType) {
		out := reflect.MakeSlice(fv.Type(), len(strs), len(strs))
		for i, s := range strs {
			ev := out.Index(i)
			if ev.Kind() == reflect.Pointer {
				ev.Set(reflect.New(ev.Type().Elem()))
				ev = ev.Elem()
			}
			if err := parseScalar(ev, s); err != nil {
				return err
			}
		}
		fv.Set(out)
		return nil
	}
	if len(strs) == 0 {
		return nil
	}
	return parseScalar(fv, strs[0])
}

func parseScalar(v reflect.Value, s string) error {
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(s))
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("unsupported type %s", v.Type())
		}
		v.SetBytes([]byte(s))
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}
