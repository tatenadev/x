package gsheets

import (
	"context"
	"fmt"
	"reflect"

	"google.golang.org/api/sheets/v4"
)

// Writer provides functionality to write data to Google Sheets.
type Writer struct {
	service       *sheets.Service
	spreadsheetID string
}

// NewWriter creates a new Writer for a given spreadsheet.
func NewWriter(client *sheets.Service, spreadsheetID string) *Writer {
	return &Writer{
		service:       client,
		spreadsheetID: spreadsheetID,
	}
}

// WriteData writes a slice of data to a specified sheet and range.
// The data is expected to be a slice of structs, and the headers will be extracted
// from the field names of the first struct.
func (w *Writer) WriteData(ctx context.Context, sheetName, valueInputOption, insertDataOption string, data any) error {
	valueRange, err := toValueRange(data)
	if err != nil {
		return fmt.Errorf("failed to convert data to ValueRange: %w", err)
	}

	if len(valueRange.Values) == 0 {
		return nil // Nothing to write
	}

	// Determine the range to write to. If headers are present, we assume the first row is headers.
	// We'll write starting from A1 and let Google Sheets handle appending or overwriting based on options.
	writeRange := sheetName + "!A1" // Default to A1, Google Sheets API will append if rows exist and option is APPEND

	_, err = w.service.Spreadsheets.Values.Append(w.spreadsheetID, writeRange, valueRange).
		ValueInputOption(valueInputOption).
		InsertDataOption(insertDataOption).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("unable to write data to sheet %s: %w", sheetName, err)
	}
	return nil
}

func toValueRange(data any) (*sheets.ValueRange, error) {
	val := reflect.ValueOf(data)
	if val.Kind() != reflect.Slice {
		return nil, fmt.Errorf("toValueRange expects a slice, got %T", data)
	}

	if val.Len() == 0 {
		return &sheets.ValueRange{Values: [][]any{}}, nil
	}

	var headers []any
	var rows [][]any

	// Extract headers from the first struct's field names
	firstElem := val.Index(0)
	if firstElem.Kind() == reflect.Pointer {
		firstElem = firstElem.Elem()
	}

	if firstElem.Kind() == reflect.Struct {
		for i := 0; i < firstElem.NumField(); i++ {
			field := firstElem.Type().Field(i)
			// Use the JSON tag if available, otherwise the field name
			jsonTag := field.Tag.Get("json")
			if jsonTag != "" && jsonTag != "-" {
				headers = append(headers, jsonTag)
			} else {
				headers = append(headers, field.Name)
			}
		}
		rows = append(rows, headers)
	}

	// Extract values for each row
	for i := 0; i < val.Len(); i++ {
		elem := val.Index(i)
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}

		if elem.Kind() == reflect.Struct {
			var row []any
			for _, field := range elem.Fields() {
				row = append(row, field.Interface())
			}
			rows = append(rows, row)
		} else if elem.Kind() == reflect.Slice || elem.Kind() == reflect.Array {
			// If it's a slice/array of interfaces, just append it directly
			var row []any
			for k := 0; k < elem.Len(); k++ {
				row = append(row, elem.Index(k).Interface())
			}
			rows = append(rows, row)
		} else {
			return nil, fmt.Errorf("unsupported element type in slice: %T", elem.Interface())
		}
	}

	return &sheets.ValueRange{Values: rows}, nil
}
