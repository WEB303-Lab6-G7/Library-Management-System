// Package svc has small HTTP helpers every service uses.
package svc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Env reads an environment variable with a default.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvInt reads an integer environment variable with a default.
func EnvInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// JSON writes v as JSON with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes {"error": msg}.
func Error(w http.ResponseWriter, status int, msg string, args ...any) {
	JSON(w, status, map[string]string{"error": fmt.Sprintf(msg, args...)})
}

// Decode reads a JSON request body.
func Decode(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// PathID parses an integer path value such as {id}.
func PathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

// Serve starts the HTTP server with request logging and a /health endpoint.
func Serve(service, addr string, mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"service": service, "status": "ok"})
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		mux.ServeHTTP(rec, r)
		if r.URL.Path != "/health" {
			log.Printf("[http] %s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
	log.Printf("[%s] HTTP listening on %s", service, addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Client is a shared HTTP client for the few synchronous calls between services.
var Client = &http.Client{Timeout: 3 * time.Second}

// GetJSON performs a GET and decodes the JSON response. A non-2xx status is an error
// carrying the service's error message.
func GetJSON(url string, v any) (int, error) {
	resp, err := Client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, errors.New(e.Error)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(v)
}
