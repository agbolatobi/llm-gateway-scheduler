//go:build linux

package api

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/auth"
)

type unreadableBody struct{}

func (unreadableBody) Read([]byte) (int, error) { panic("unauthorized request body was read") }
func (unreadableBody) Close() error             { return nil }

func TestAuthenticationOnBothEndpoints(t *testing.T) {
	store, err := auth.Open(filepath.Join(t.TempDir(), "keys"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create("alice", "http://127.0.0.1:8000")
	if err != nil {
		t.Fatal(err)
	}
	token, _ := store.Show("")
	handler := NewHandler(store)
	for _, endpoint := range []struct{ method, path string }{{"POST", "/v1/chat/completions"}, {"GET", "/v1/models"}} {
		t.Run(endpoint.path, func(t *testing.T) {
			for _, header := range []string{"", "Basic " + token, "Bearer wrong", "Bearer", "Bearer " + token + " extra"} {
				request := httptest.NewRequest(endpoint.method, endpoint.path, nil)
				request.Body = unreadableBody{}
				if header != "" {
					request.Header.Set("Authorization", header)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 401 || response.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("invalid credentials must receive 401 challenge")
				}
				if strings.Contains(response.Body.String(), token) {
					t.Fatal("response leaked credentials")
				}
			}
			request := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(validBody))
			request.Header.Set("Authorization", "bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 501 {
				t.Fatalf("valid key got %d", response.Code)
			}
			request = httptest.NewRequest(endpoint.method, endpoint.path+"?api_key="+token, nil)
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 401 {
				t.Fatal("query-string keys must not authenticate")
			}
			request.Header.Add("Authorization", "Bearer "+token)
			request.Header.Add("Authorization", "Bearer "+token)
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 401 {
				t.Fatal("duplicate authorization headers must fail")
			}
		})
	}
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	authenticated, ok := authenticate(httptest.NewRecorder(), request, store)
	principal, present := PrincipalFromContext(authenticated.Context())
	if !ok || !present || principal.UserID != "alice" || principal.KeyID != record.ID {
		t.Fatal("verified user missing from context")
	}
	if err := store.Revoke(record.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatal("revoked key accepted")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/health", nil))
	if response.Code != 200 {
		t.Fatal("health must remain public")
	}
}

type brokenVerifier struct{}

func (brokenVerifier) Verify(string) (auth.Principal, error) {
	return auth.Principal{}, errors.New("sensitive diagnostic")
}
func TestUnavailableVerifierFailsClosed(t *testing.T) {
	for _, verifier := range []Authenticator{nil, brokenVerifier{}} {
		request := httptest.NewRequest("GET", "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer value")
		response := httptest.NewRecorder()
		NewHandler(verifier).ServeHTTP(response, request)
		if response.Code != 503 || strings.Contains(response.Body.String(), "sensitive") {
			t.Fatal("verifier failure must be sanitized and fail closed")
		}
	}
	store, err := auth.Open(filepath.Join(t.TempDir(), "keys"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("alice", "http://localhost:8000"); err != nil {
		t.Fatal(err)
	}
	token, _ := store.Show("")
	if err := os.WriteFile(filepath.Join(store.Dir, "api-keys.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatal("corrupt store must not allow access")
	}
}
