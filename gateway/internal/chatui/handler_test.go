package chatui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicAssetsAndSecurityHeaders(testContext *testing.T) {
	for path, asset := range publicAssets {
		response := httptest.NewRecorder()
		if !Serve(response, httptest.NewRequest("GET", path, nil)) || response.Code != 200 {
			testContext.Fatalf("asset unavailable: %s", path)
		}
		if response.Header().Get("Content-Type") != asset.contentType || response.Body.Len() == 0 {
			testContext.Fatalf("invalid asset response: %s", path)
		}
		if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "connect-src 'self'") || response.Header().Get("Referrer-Policy") != "no-referrer" {
			testContext.Fatal("missing browser security headers")
		}
		head := httptest.NewRecorder()
		Serve(head, httptest.NewRequest("HEAD", path, nil))
		if head.Body.Len() != 0 || head.Header().Get("Content-Length") != response.Header().Get("Content-Length") {
			testContext.Fatal("HEAD must return only the GET headers")
		}
	}
	for _, path := range []string{"/v1/models", "/chat/../keys.json", "/chat/missing", "/chat/index.html"} {
		if Serve(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil)) {
			testContext.Fatalf("unexpected public asset: %s", path)
		}
	}
	rejected := httptest.NewRecorder()
	Serve(rejected, httptest.NewRequest("POST", Path, nil))
	if rejected.Code != http.StatusMethodNotAllowed {
		testContext.Fatal("POST must be rejected")
	}
	redirect := httptest.NewRecorder()
	Serve(redirect, httptest.NewRequest("GET", "/chat", nil))
	if redirect.Code != 308 || redirect.Header().Get("Location") != Path {
		testContext.Fatal("missing canonical path redirect")
	}
}

func TestChatLinks(testContext *testing.T) {
	for _, testCase := range []struct{ base, override, expected string }{
		{"http://127.0.0.1:8000", "", "http://127.0.0.1:8000/chat/"},
		{"http://localhost:8010/", "", "http://localhost:8010/chat/"},
		{"http://127.0.0.1:8000", "https://chat.example.com", "https://chat.example.com/chat/"},
		{"", "https://chat.example.com/chat/", "https://chat.example.com/chat/"},
	} {
		actual, err := Link(testCase.base, testCase.override)
		if err != nil || actual != testCase.expected {
			testContext.Fatalf("link = %q, error = %v", actual, err)
		}
	}
	for _, invalid := range []string{"http://public.example.com", "javascript:alert(1)", "//example.com", "https://user:secret@example.com", "https://example.com/chat/?key=secret", "https://example.com/#secret", "https://example.com/other", "https://example.com:0", "https://example.com:99999", "https://example.com:"} {
		if _, err := Link("", invalid); err == nil {
			testContext.Fatalf("accepted invalid link: %s", invalid)
		}
	}
}
