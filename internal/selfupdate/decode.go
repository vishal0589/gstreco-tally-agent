package selfupdate

import (
	"encoding/json"
	"net/http"
)

// decodeBody reads + decodes a JSON HTTP response. Pulled into its
// own file so the rest of checker.go doesn't pull in encoding/json
// directly — keeps the file's import surface narrow.
func decodeBody(resp *http.Response, v any) error {
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(v)
}
