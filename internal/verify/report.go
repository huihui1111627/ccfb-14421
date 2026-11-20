package verify

import (
	"encoding/json"
	"os"
)

// WriteJSON serializes a verification result for the web page.
func WriteJSON(r *Result, path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
