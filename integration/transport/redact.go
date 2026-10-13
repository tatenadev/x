// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"bytes"
	"encoding/json/jsontext"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// Redacted replaces a secret in logs and errors.
const Redacted = "REDACTED"

// sensitiveHeaders are always redacted, whatever their value.
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"X-Api-Key":           true,
}

// SensitiveName reports whether a header, query parameter, form field or
// JSON member name likely carries a credential. It errs toward redacting.
func SensitiveName(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "key", "sig", "code", "auth", "authorization", "cookie", "set-cookie":
		return true
	}
	for _, s := range []string{"token", "secret", "password", "passwd", "signature", "apikey", "api_key", "api-key", "credential", "session"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// RedactURL renders u for logs and errors: userinfo is replaced and query
// values with sensitive names are masked. u itself is not modified.
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	r := u.Clone()
	if r.User != nil {
		r.User = url.User(Redacted)
	}
	if r.RawQuery != "" {
		r.RawQuery = redactValues(r.Query()).Encode()
	}
	return r.String()
}

// redactValues masks the values of sensitive names in a copy of v.
func redactValues(v url.Values) url.Values {
	out := v.Clone()
	for k := range out {
		if SensitiveName(k) {
			out[k] = []string{Redacted}
		}
	}
	return out
}

// RedactHeader returns a copy of h with credential headers masked.
func RedactHeader(h http.Header) http.Header {
	out := h.Clone()
	for k := range out {
		if sensitiveHeaders[http.CanonicalHeaderKey(k)] || SensitiveName(k) {
			out[k] = []string{Redacted}
		}
	}
	return out
}

// redactBody renders a captured body prefix for logs. Only formats whose
// secrets can be masked are rendered: JSON (sensitive members) and forms
// (sensitive fields). Anything else is described, not shown.
func redactBody(contentType string, b []byte, truncated bool) string {
	mt, _, _ := mime.ParseMediaType(contentType)
	var s string
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		s, truncated = redactJSON(b, truncated)
	case mt == "application/x-www-form-urlencoded":
		v, err := url.ParseQuery(string(b))
		if err != nil {
			return "<unparsable form body omitted>"
		}
		s = redactValues(v).Encode()
	default:
		return "<" + contentTypeLabel(mt) + " body omitted>"
	}
	if truncated {
		s += "…(truncated)"
	}
	return s
}

func contentTypeLabel(mt string) string {
	if mt == "" {
		return "untyped"
	}
	return mt
}

// redactJSON re-encodes b token by token, replacing the value of every
// sensitive member with "REDACTED". On cut-off or malformed input it keeps
// what it could verify, closes the open objects and arrays (with "…" for a
// missing value), and reports the result as truncated.
func redactJSON(b []byte, truncated bool) (string, bool) {
	lenient := []jsontext.Options{jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)}
	dec := jsontext.NewDecoder(bytes.NewReader(b), lenient...)
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out, lenient...)

	// The encoder only writes out a complete top-level value, so a cut
	// must be closed off before anything is emitted.
	closeOff := func() (string, bool) {
		for d := enc.StackDepth(); d > 0; d-- {
			kind, n := enc.StackIndex(d)
			var err error
			if kind == jsontext.KindBeginObject {
				if n%2 == 1 {
					err = enc.WriteToken(jsontext.String("…"))
				}
				if err == nil {
					err = enc.WriteToken(jsontext.EndObject)
				}
			} else {
				err = enc.WriteToken(jsontext.EndArray)
			}
			if err != nil {
				return "", true
			}
		}
		return strings.TrimSpace(out.String()), true
	}

	for {
		kind, n := dec.StackIndex(dec.StackDepth())
		atName := kind == jsontext.KindBeginObject && n%2 == 0
		tok, err := dec.ReadToken()
		if err != nil {
			if dec.StackDepth() == 0 && out.Len() > 0 { // io.EOF after a complete value
				return strings.TrimSpace(out.String()), truncated
			}
			return closeOff()
		}
		if err := enc.WriteToken(tok); err != nil {
			return closeOff()
		}
		if atName && SensitiveName(tok.String()) {
			if err := dec.SkipValue(); err != nil {
				return closeOff()
			}
			if err := enc.WriteToken(jsontext.String(Redacted)); err != nil {
				return closeOff()
			}
		}
	}
}
