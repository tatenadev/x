// Copyright (c) 2024-2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package fsio_test

import (
	"os"
	"testing"
	"testing/fstest"

	"github.com/tatenadev/x"
	"github.com/tatenadev/x/fsio"
)

func TestGlob(t *testing.T) {
	fs := fstest.MapFS{
		"data/file1.json":  &fstest.MapFile{Data: []byte(`{}`)},
		"data/file2.json":  &fstest.MapFile{Data: []byte(`{}`)},
		"data/file3.txt":   &fstest.MapFile{Data: []byte(`text`)},
		"other/file4.json": &fstest.MapFile{Data: []byte(`{}`)},
	}

	tests := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			name:    "match JSON files in data",
			pattern: "data/*.json",
			want:    []string{"data/file1.json", "data/file2.json"},
		},
		{
			name:    "match all JSON files",
			pattern: "*/*.json",
			want:    []string{"data/file1.json", "data/file2.json", "other/file4.json"},
		},
		{
			name:    "no matches",
			pattern: "*.xml",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for match := range fsio.Glob(fs, tt.pattern) {
				got = append(got, match)
			}
			if len(got) != len(tt.want) {
				t.Errorf("Glob() got %d matches, want %d", len(got), len(tt.want))
				return
			}
			for i, want := range tt.want {
				if got[i] != want {
					t.Errorf("Glob() got[%d] = %v, want %v", i, got[i], want)
				}
			}
		})
	}
}

func TestGlob_InvalidPattern(t *testing.T) {
	fs := fstest.MapFS{}

	// Glob logs a bad pattern and yields nothing; it does not panic.
	for got := range fsio.Glob(fs, "[") {
		t.Errorf("Glob() yielded %q for invalid pattern", got)
	}
}

func TestDynamicWriter_Stdout(t *testing.T) {
	tests := []string{"-", "stdout"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			w, err := x.DynamicWriter(name, false)
			if err != nil {
				t.Fatalf("DynamicWriter() error = %v", err)
			}
			// Should return os.Stdout, we can't directly compare, but we know it shouldn't be nil
			if w == nil {
				t.Error("DynamicWriter() returned nil for stdout")
			}
			// Don't close stdout
		})
	}
}

func TestDynamicWriter_Stderr(t *testing.T) {
	tests := []string{"=", "stderr"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			w, err := x.DynamicWriter(name, false)
			if err != nil {
				t.Fatalf("DynamicWriter() error = %v", err)
			}
			if w == nil {
				t.Error("DynamicWriter() returned nil for stderr")
			}
			// Don't close stderr
		})
	}
}

func TestDynamicWriter_File(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := tmpDir + "/output.txt"

	w, err := x.DynamicWriter(filePath, false)
	if err != nil {
		t.Fatalf("DynamicWriter() error = %v", err)
	}
	defer w.Close()

	_, err = w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func TestDynamicWriter_FileAppend(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := tmpDir + "/append.txt"

	// Write first content
	w1, err := x.DynamicWriter(filePath, false)
	if err != nil {
		t.Fatalf("DynamicWriter() error = %v", err)
	}
	_, _ = w1.Write([]byte("first"))
	w1.Close()

	// Append second content
	w2, err := x.DynamicWriter(filePath, true)
	if err != nil {
		t.Fatalf("DynamicWriter() append error = %v", err)
	}
	_, _ = w2.Write([]byte("second"))
	w2.Close()

	// Verify content using os.ReadFile
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "firstsecond" {
		t.Errorf("DynamicWriter append got %q, want %q", string(data), "firstsecond")
	}
}

func TestDynamicWriter_FileTruncate(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := tmpDir + "/truncate.txt"

	// Write first content
	w1, err := x.DynamicWriter(filePath, false)
	if err != nil {
		t.Fatalf("DynamicWriter() error = %v", err)
	}
	_, _ = w1.Write([]byte("first content that is long"))
	w1.Close()

	// Truncate and write second content
	w2, err := x.DynamicWriter(filePath, false)
	if err != nil {
		t.Fatalf("DynamicWriter() truncate error = %v", err)
	}
	_, _ = w2.Write([]byte("short"))
	w2.Close()
}

func TestDynamicWriter_InvalidPath(t *testing.T) {
	_, err := x.DynamicWriter("/nonexistent/path/to/file.txt", false)
	if err == nil {
		t.Error("DynamicWriter() expected error for invalid path")
	}
}
