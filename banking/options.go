package banking

import (
	"fmt"
	"io"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

// Encoding names accepted by WithEncoding and Config.Encoding. See
// DecodeReader for how each is handled.
const (
	EncodingUTF8        = "utf-8"
	EncodingWindows1250 = "windows-1250"
	EncodingWindows1252 = "windows-1252"
	EncodingISO88591    = "iso-8859-1"
	EncodingISO88592    = "iso-8859-2"
	EncodingISO885915   = "iso-8859-15"
)

// defaultDateFormat is the time.Parse layout Config falls back to when no
// WithDateFormat option is given.
const defaultDateFormat = "2006-01-02"

// defaultDelimiter is the field delimiter Config falls back to when no
// WithDelimiter option is given.
const defaultDelimiter = ','

// errPrefix identifies errors originating from this package.
const errPrefix = "banking"

// Config holds the options shared across statement format parsers. Not every
// field is meaningful to every format; each Parser implementation documents
// which ones it honors.
type Config struct {
	// DateFormat is the time.Parse layout used for dates in formats that
	// don't have a fixed date grammar (e.g. CSV).
	DateFormat string
	// Encoding is the source character encoding, e.g. EncodingUTF8 (default),
	// EncodingWindows1250, EncodingISO88592. See DecodeReader for supported names.
	Encoding string
	// SkipHeaderLines is the number of leading lines to discard before
	// parsing begins (e.g. a CSV header row).
	SkipHeaderLines int
	// Delimiter is the field delimiter for delimited formats. Defaults to ','.
	Delimiter rune
}

// Option configures a Config.
type Option func(*Config)

// WithDateFormat sets the time.Parse layout used to parse dates.
func WithDateFormat(layout string) Option {
	return func(c *Config) { c.DateFormat = layout }
}

// WithEncoding sets the source character encoding. See DecodeReader for the
// set of recognized names.
func WithEncoding(name string) Option {
	return func(c *Config) { c.Encoding = name }
}

// WithSkipHeaderLines sets the number of leading lines to discard.
func WithSkipHeaderLines(n int) Option {
	return func(c *Config) { c.SkipHeaderLines = n }
}

// WithDelimiter sets the field delimiter for delimited formats.
func WithDelimiter(r rune) Option {
	return func(c *Config) { c.Delimiter = r }
}

// NewConfig builds a Config from opts, applying defaults for anything left
// unset.
func NewConfig(opts ...Option) Config {
	cfg := Config{
		DateFormat: defaultDateFormat,
		Encoding:   EncodingUTF8,
		Delimiter:  defaultDelimiter,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// encodings maps the names accepted by WithEncoding to their charmap.Charmap.
// Names are matched case-insensitively; EncodingUTF8 and "" both mean no
// conversion.
var encodings = map[string]*charmap.Charmap{
	EncodingWindows1250: charmap.Windows1250,
	EncodingWindows1252: charmap.Windows1252,
	EncodingISO88591:    charmap.ISO8859_1,
	EncodingISO88592:    charmap.ISO8859_2,
	EncodingISO885915:   charmap.ISO8859_15,
}

// DecodeReader wraps r so that bytes read from it are transcoded from
// encodingName to UTF-8. An empty name or EncodingUTF8 returns r unchanged.
func DecodeReader(r io.Reader, encodingName string) (io.Reader, error) {
	if encodingName == "" || encodingName == EncodingUTF8 {
		return r, nil
	}
	cm, ok := encodings[encodingName]
	if !ok {
		return nil, fmt.Errorf("%s: unsupported encoding %q", errPrefix, encodingName)
	}
	var enc encoding.Encoding = cm
	return transform.NewReader(r, enc.NewDecoder()), nil
}
