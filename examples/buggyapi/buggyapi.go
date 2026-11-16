// Package buggyapi is a small users API with deliberate, realistic bugs. It
// exists so you can watch ReplayHarness find them:
//
//	go run ./examples/buggyapi/cmd/buggy-api
//	replayharness replay ./examples/buggyapi/replays.yaml --target http://127.0.0.1:8080
//	replayharness run ./examples/buggyapi --target http://127.0.0.1:8080
//
// Every bug is marked with a BUG comment. openapi.yaml describes what the API
// is supposed to do.
package buggyapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

//go:embed openapi.yaml
var spec []byte

type user struct {
	ID    int
	Name  string
	Email string
	Age   int
}

type server struct {
	mu    sync.Mutex
	users []user
}

// Handler returns the API.
func Handler() http.Handler {
	s := &server{users: []user{
		{1, "Ada Lovelace", "ada@example.com", 36},
		{2, "Alan Turing", "alan@example.com", 41},
		{3, "Grace Hopper", "grace@example.com", 85},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(spec)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("POST /users", debugErrors(s.createUser))
	mux.HandleFunc("GET /users/{id}", debugErrors(s.getUser))
	mux.HandleFunc("DELETE /users/{id}", debugErrors(s.deleteUser))
	// No recovery middleware here: a panic drops the connection.
	mux.HandleFunc("GET /search", s.search)
	// BUG: this route is not in openapi.yaml.
	mux.HandleFunc("GET /admin/stats", s.stats)
	return mux
}

// debugErrors turns panics into 500 responses.
// BUG: it sends the stack trace to the client.
func debugErrors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "panic: %v\n\n%s", rec, debug.Stack())
			}
		}()
		h(w, r)
	}
}

func (s *server) createUser(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON"))
		return
	}
	name, ok := body["name"].(string)
	if !ok || name == "" {
		writeJSON(w, http.StatusBadRequest, errBody("name is required"))
		return
	}
	// BUG: the spec requires email, but nothing checks that it is there or
	// that it is an email address. BUG: name's maxLength (50) is not enforced.
	email, _ := body["email"].(string)
	// BUG: unchecked type assertion. A missing, null or non-numeric age panics.
	age := int(body["age"].(float64))
	if age < 0 || age > 150 {
		writeJSON(w, http.StatusBadRequest, errBody("age must be between 0 and 150"))
		return
	}
	s.mu.Lock()
	u := user{ID: len(s.users) + 1, Name: name, Email: email, Age: age}
	s.users = append(s.users, u)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "age": u.Age})
}

func (s *server) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("id must be an integer"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id > len(s.users) {
		writeJSON(w, http.StatusNotFound, errBody("user not found"))
		return
	}
	u := s.users[id-1] // BUG: no check for id < 1, so 0 and negative ids panic.
	// BUG: age is documented as an integer but sent as a string.
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "age": strconv.Itoa(u.Age)})
}

func (s *server) deleteUser(w http.ResponseWriter, r *http.Request) {
	// BUG: the spec requires a bearer token, but the handler never checks one.
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("id must be an integer"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || id > len(s.users) {
		writeJSON(w, http.StatusNotFound, errBody("user not found"))
		return
	}
	// BUG: answers 204 but never removes the user.
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("limit must be an integer"))
			return
		}
		limit = n // BUG: limit is documented as 1..100 but never range-checked.
	}
	prefix := strings.ToLower(q[:3]) // BUG: panics on queries shorter than 3 characters.
	s.mu.Lock()
	results := []map[string]any{}
	for _, u := range s.users {
		if strings.Contains(strings.ToLower(u.Name), prefix) {
			results = append(results, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "age": u.Age})
		}
	}
	s.mu.Unlock()
	if limit < len(results) {
		results = results[:limit] // BUG: a negative limit panics here.
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := len(s.users)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"users": n})
}

func errBody(msg string) map[string]any { return map[string]any{"error": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
