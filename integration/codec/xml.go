// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package codec

import (
	"encoding/xml"
	"io"
)

// XML is the encoding/xml codec.
type XML struct{}

// ContentType returns "application/xml".
func (XML) ContentType() string { return ContentTypeXML }

// Encode writes v to w as XML, without a declaration header.
func (XML) Encode(w io.Writer, v any) error {
	return xml.NewEncoder(w).Encode(v)
}

// Decode reads one XML element from r into v.
func (XML) Decode(r io.Reader, v any) error {
	return xml.NewDecoder(r).Decode(v)
}
