// Package output formats pipeline results for the CLI's stdout.
package output

import (
	"encoding/json"
	"io"

	"ghcall/internal/pipeline"
)

// WriteJSON writes results as an indented JSON array.
func WriteJSON(w io.Writer, results []pipeline.FilterResult) error {
	if results == nil {
		results = []pipeline.FilterResult{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}
