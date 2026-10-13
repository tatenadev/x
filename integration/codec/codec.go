// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package codec encodes request bodies and decodes response bodies for the
// integration layer. Encoding lives here and never in a transport.
package codec

import (
	"io"
	"mime"
	"strings"
)

// Media types of the built-in codecs.
const (
	ContentTypeJSON = "application/json"
	ContentTypeXML  = "application/xml"
	ContentTypeForm = "application/x-www-form-urlencoded"
)

// Codec converts between Go values and one wire format. It is deliberately
// non-generic so it can be stored and swapped like any interface value.
type Codec interface {
	// ContentType is the media type for Content-Type and Accept headers.
	ContentType() string
	// Encode writes v to w.
	Encode(w io.Writer, v any) error
	// Decode reads one value from r into v, which must be a non-nil pointer.
	Decode(r io.Reader, v any) error
}

// ForContentType returns the built-in codec for a Content-Type header
// value. Parameters such as charset are ignored, and structured suffixes
// ("application/problem+json", "application/atom+xml") map to their base
// format. JSON uses v2 defaults.
func ForContentType(contentType string) (Codec, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, false
	}
	switch {
	case mt == ContentTypeJSON || strings.HasSuffix(mt, "+json"):
		return JSON{}, true
	case mt == ContentTypeXML || mt == "text/xml" || strings.HasSuffix(mt, "+xml"):
		return XML{}, true
	case mt == ContentTypeForm:
		return Form{}, true
	}
	return nil, false
}
