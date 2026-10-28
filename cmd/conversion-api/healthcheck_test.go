package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheckValidatesStatusAndBody(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"healthy", 200, `{"status":"ok"}`, true},
		{"unavailable", 503, `{"status":"ok"}`, false},
		{"not-found", 404, `{"error":"not_found"}`, false},
		{"wrong-status", 201, `{"status":"ok"}`, false},
		{"wrong-body", 200, `{"status":"starting"}`, false},
		{"html", 200, `<html>ok</html>`, false},
		{"trailing-json", 200, `{"status":"ok"} {}`, false},
		{"too-large", 200, strings.Repeat(" ", 4097) + `{"status":"ok"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
					t.Error("health probe must be an unauthenticated GET /healthz")
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			err := checkHealth(server.URL + "/healthz")
			if (err == nil) != test.ok {
				t.Fatalf("unexpected health result: %v", err)
			}
		})
	}
}

func TestHealthcheckDoesNotFollowRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/healthz", http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	if checkHealth(server.URL+"/redirect") == nil {
		t.Fatal("redirect must not be treated as a healthy response")
	}
}

func TestHealthcheckFailsWhenServiceIsStopped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL + "/healthz"
	server.Close()
	if checkHealth(endpoint) == nil {
		t.Fatal("a stopped service must fail the health check")
	}
}
