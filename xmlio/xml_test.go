// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package xmlio_test

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/tatenadev/x/xmlio"
)

func TestWriteElements_FormatsCommentsAndDirectives(t *testing.T) {
	in := `<root><a><!--note--><?pi data?><!DOCTYPE x>text</a></root>`
	var out strings.Builder
	if err := xmlio.WriteElements("a", strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"<!--note-->", "<?pi data?>", "<!DOCTYPE x>"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "%s") {
		t.Errorf("output %q contains an unformatted verb", got)
	}
}

func TestTokens_StopsOnSyntaxError(t *testing.T) {
	var n int
	for range xmlio.Tokens(xml.NewDecoder(strings.NewReader(`<a><b></a>`)), false) {
		n++
		if n > 100 {
			t.Fatal("Tokens kept yielding after a syntax error")
		}
	}
}
