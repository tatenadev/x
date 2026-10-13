// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package codec

import (
	"errors"
	"fmt"
	"io"
	"iter"

	"encoding/json/jsontext"
	"encoding/json/v2"
)

// JSON is the encoding/json/v2 codec. The zero value keeps v2's strict
// defaults: duplicate object names and invalid UTF-8 are rejected, and
// names match case-sensitively. Relax them per upstream with [NewJSON].
type JSON struct {
	opts []json.Options
}

// NewJSON returns a JSON codec that applies opts on every encode and
// decode, for example jsontext.AllowDuplicateNames(true) for an upstream
// known to repeat keys.
func NewJSON(opts ...json.Options) JSON {
	return JSON{opts: append([]json.Options(nil), opts...)}
}

// ContentType returns "application/json".
func (JSON) ContentType() string { return ContentTypeJSON }

// Encode writes v to w with json.MarshalWrite.
func (c JSON) Encode(w io.Writer, v any) error {
	return json.MarshalWrite(w, v, c.opts...)
}

// Decode reads exactly one JSON value from r into v with
// json.UnmarshalRead; trailing data is an error.
func (c JSON) Decode(r io.Reader, v any) error {
	return json.UnmarshalRead(r, v, c.opts...)
}

// Each streams the elements of a top-level JSON array in r, decoding one
// element at a time, so a consumer that stops early never decodes the rest.
// A top-level null is an empty array. A decode error is yielded as
// (zero, err) and ends the sequence.
func (c JSON) Each[T any](r io.Reader) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		dec := jsontext.NewDecoder(r, c.opts...)
		fail := func(err error) { yield(zero, err) }

		switch k := dec.PeekKind(); k {
		case jsontext.KindNull, jsontext.KindBeginArray:
			if _, err := dec.ReadToken(); err != nil {
				fail(err)
				return
			}
			if k == jsontext.KindNull {
				if err := expectEOF(dec); err != nil {
					fail(err)
				}
				return
			}
		default:
			if _, err := dec.ReadToken(); err != nil {
				fail(err) // malformed or empty input
				return
			}
			fail(fmt.Errorf("codec: json: want an array, got %v", k))
			return
		}

		for dec.PeekKind() != jsontext.KindEndArray {
			var v T
			if err := json.UnmarshalDecode(dec, &v, c.opts...); err != nil {
				fail(err)
				return
			}
			if !yield(v, nil) {
				return
			}
		}
		if _, err := dec.ReadToken(); err != nil { // the closing ']'
			fail(err)
			return
		}
		if err := expectEOF(dec); err != nil {
			fail(err)
		}
	}
}

// expectEOF reports an error if dec has another top-level value.
func expectEOF(dec *jsontext.Decoder) error {
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("codec: json: unexpected data after top-level value")
		}
		return err
	}
	return nil
}
