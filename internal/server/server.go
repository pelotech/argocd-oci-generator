// Package server serves the Argo CD plugin generator API.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"

	"github.com/pelotech/argocd-oci-generator/internal/generator"
)

// Source answers a poll of the set; the generator is one.
type Source interface {
	Parameters(ctx context.Context, in generator.Inputs) ([]generator.Entry, error)
}

// MaxBody bounds the request body; a set's parameters are a few hundred bytes.
const MaxBody = 1 << 20

// Handler serves POST /api/v1/getparams.execute to a caller that presents the bearer token.
func Handler(source Source, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/getparams.execute", func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var body struct {
			Input struct {
				Parameters generator.Inputs `json:"parameters"`
			} `json:"input"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		entries, err := source.Parameters(r.Context(), body.Input.Parameters)
		if err != nil { // any upstream failure is a 502; Argo CD keeps the last parameters
			log.Printf("error: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"parameters": entries}})
	})
	return mux
}
