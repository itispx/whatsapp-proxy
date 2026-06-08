// receiver is a lightweight webhook sink used by the dev environment.
// It stores incoming POST bodies in memory and exposes them for test assertions.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

var (
	mu        sync.Mutex
	callbacks []json.RawMessage
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}

	mux := http.NewServeMux()

	// Proxy delivers status callbacks here.
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		callbacks = append(callbacks, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// Tests call this to retrieve received callbacks.
	mux.HandleFunc("GET /callbacks", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if callbacks == nil {
			w.Write([]byte("[]"))
			return
		}
		json.NewEncoder(w).Encode(callbacks)
	})

	// Tests call this to clear callbacks between runs.
	mux.HandleFunc("DELETE /callbacks", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callbacks = nil
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	fmt.Printf("receiver listening on :%s\n", port)
	http.ListenAndServe(":"+port, mux)
}
