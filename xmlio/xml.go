// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package xmlio

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"iter"
	"log/slog"
	"regexp"
	"strings"

	"charm.land/log/v2"
)

func XMLDicts(r io.Reader) iter.Seq2[string, string] {
	var xpath []string
	return func(yield func(string, string) bool) {
		for t := range Tokens(xml.NewDecoder(r), false) {
			switch e := t.(type) {
			case xml.StartElement:
				xpath = append(xpath, e.Name.Local)
			case xml.EndElement:
				xpath = xpath[:len(xpath)-1]
			case xml.CharData:
				if !yield(strings.Join(xpath, " > "), string(e)) {
					return
				}
			default:
				log.Printf("%T", e)
			}
		}
	}
}

// Tokens
// TODO: ensure proper handling of namespaces
func TrimXML(r io.Reader, w io.Writer, asHTML, rawToken bool) (err error) {
	prevElemType := ""
	var xpath []string
	dec := xml.NewDecoder(r)
	if asHTML {
		dec.Strict = false
		dec.AutoClose = xml.HTMLAutoClose
		dec.Entity = xml.HTMLEntity
	}
	for t := range Tokens(dec, rawToken) {
		switch el := t.(type) {
		case xml.ProcInst:
			_, err = fmt.Fprintf(w, "<?%s %s?>\n", el.Target, el.Inst)
		case xml.Directive:
			_, err = fmt.Fprintf(w, "<!%s>\n", el)
		case xml.Comment:
			_, err = fmt.Fprintf(w, "<!--%s-->\n", el)
		case xml.StartElement:
			// render start-element
			if prevElemType == "xml.StartElement" || prevElemType == "xml.EndElement" {
				if _, err = fmt.Fprint(w, "\n", strings.Repeat("\t", len(xpath))); err != nil {
					break
				}
			}
			_, err = fmt.Fprint(w, startElement(el, "\n"+strings.Repeat("\t", len(xpath)), strings.TrimSpace, html.EscapeString, NormalizeSpaces)...)
			// increase nesting level
			xpath = append(xpath, el.Name.Local)
		case xml.EndElement:
			// decrease nesting level
			xpath = xpath[:len(xpath)-1]
			// render end-element
			if prevElemType == "xml.StartElement" || prevElemType == "xml.EndElement" {
				if _, err = fmt.Fprint(w, "\n", strings.Repeat("\t", len(xpath))); err != nil {
					break
				}
			}
			if el.Name.Space != "" {
				el.Name.Space += ":"
			}
			_, err = fmt.Fprint(w, []any{"</", el.Name.Space, el.Name.Local, ">"}...)
		case xml.CharData:
			err = xml.EscapeText(w, bytes.TrimSpace(reSpaces.ReplaceAll(el, []byte(" "))))
		default:
			log.Printf("%T", el)
		}
		if err != nil {
			return err
		}
		prevElemType = fmt.Sprintf("%T", t)
	}
	return nil
}

func WriteElements(element string, r io.Reader, w io.Writer) (err error) {
	split := strings.Split(element, "/")
	name := split[len(split)-1]
	var enabled bool
	var xpath []string
	for t := range Tokens(xml.NewDecoder(r), false) {
		switch el := t.(type) {
		case xml.ProcInst:
			_, err = fPrintF(enabled, w, fmt.Sprintf("<?%s %s?>", el.Target, el.Inst))
		case xml.Directive:
			_, err = fPrintF(enabled, w, fmt.Sprintf("<!%s>", el))
		case xml.Comment:
			_, err = fPrintF(enabled, w, fmt.Sprintf("<!--%s-->", el))
		case xml.CharData:
			err = escapeText(enabled, w, bytes.TrimSpace(reSpaces.ReplaceAll(el, []byte(" "))))
		case xml.StartElement:
			// start capturing
			if el.Name.Local == name && strings.HasSuffix(strings.Join(append(xpath, el.Name.Local), "/"), element) {
				enabled = true
			}
			// render
			_, err = fPrintF(enabled, w, startElement(el, " ", strings.TrimSpace, html.EscapeString, NormalizeSpaces)...)
			// increase nesting level
			xpath = append(xpath, el.Name.Local)
		case xml.EndElement:
			// decrease nesting level
			xpath = xpath[:len(xpath)-1]
			// render
			if el.Name.Space != "" {
				el.Name.Space += ":"
			}
			_, err = fPrintF(enabled, w, []any{"</", el.Name.Space, el.Name.Local, ">"}...)
			// stop capturing
			if el.Name.Local == name && strings.HasSuffix(strings.Join(append(xpath, el.Name.Local), "/"), element) {
				_, err = fPrintF(enabled, w, "\n")
				enabled = false
			}
		default:
			log.Printf("%T", el)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func startElement(el xml.StartElement, spacing string, fns ...func(string) string) []any {
	if el.Name.Space != "" {
		el.Name.Space += ":"
	}
	ret := []any{"<", el.Name.Space, el.Name.Local}
	for _, a := range el.Attr {
		if a.Name.Space != "" {
			a.Name.Space += ":"
		}
		for _, fn := range fns {
			a.Value = fn(a.Value)
		}
		ret = append(ret, spacing, a.Name.Space, a.Name.Local, `="`, a.Value, `"`)
	}
	return append(ret, ">")
}

func escapeText(enabled bool, w io.Writer, s []byte) error {
	if !enabled {
		return nil
	}
	return xml.EscapeText(w, s)
}

func fPrintF(enabled bool, w io.Writer, a ...any) (n int, err error) {
	if !enabled {
		return 0, nil
	}
	return fmt.Fprint(w, a...)
}

var reSpaces = regexp.MustCompile(`\s+`)

func NormalizeSpaces(input string) string {
	return strings.TrimSpace(reSpaces.ReplaceAllString(input, " "))
}

func Tokens(dec *xml.Decoder, raw bool) iter.Seq[xml.Token] {
	token := dec.Token
	if raw {
		token = dec.RawToken
	}
	var cd xml.CharData
	return func(yield func(xml.Token) bool) {
		for {
			t, err := token()
			if err != nil {
				// Stop on any error, not just EOF: the decoder keeps
				// returning the same syntax error, so continuing would spin.
				if !errors.Is(err, io.EOF) {
					slog.Error("errored", "err", err)
				}
				if len(cd) != 0 { // there is leftover data
					// yield (no use to check for return value)
					yield(cd)
				}
				return
			}

			if c, ok := t.(xml.CharData); ok {
				cd = append(cd, bytes.TrimSpace(c)...)
				continue
			}

			if len(cd) != 0 {
				// yield any captured data
				if !yield(cd) {
					return
				}
				// reset data buffer
				cd = []byte{}
			}

			// yield current token
			if !yield(t) {
				return
			}
		}
	}
}
