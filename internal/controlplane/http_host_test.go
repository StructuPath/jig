package controlplane

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadRoutesFenceReboundHostsWithoutRequiringOrigin(t *testing.T) {
	store, _ := newTestStore(t)
	handler := NewHandler(store, "test-ui-token", nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, origin := range []string{"", "http://evil.test:8383"} {
			for _, path := range []string{"/api/definitions", "/api/runs", "/api/workers", "/api/health"} {
				request := httptest.NewRequest(method, "http://evil.test:8383"+path, nil)
				request.Header.Set("Origin", origin)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "untrusted_host_authority") {
					t.Fatalf("%s %s origin=%q: %d %s", method, path, origin, response.Code, response.Body.String())
				}
			}
		}
	}
	for _, host := range []string{"127.0.0.1:8383", "localhost:8383", "[::1]:8383"} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/health", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("trusted Host %s: %d", host, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://evil.test/api/health", nil)
	request.Header.Set("X-Jig-UI-Token", "test-ui-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("explicit UI token: %d", response.Code)
	}
}

func TestReadHostMustMatchListenerPortAndExplicitNonLoopbackIP(t *testing.T) {
	store, _ := newTestStore(t)
	handler := NewHandler(store, "", nil)
	for _, test := range []struct {
		host string
		want int
	}{
		{"127.0.0.1:8383", http.StatusOK},
		{"127.0.0.1:9000", http.StatusForbidden},
		{"192.0.2.10:8383", http.StatusOK},
		{"192.0.2.11:8383", http.StatusForbidden},
		{"evil.test:8383", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/api/health", nil)
		request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8383}))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("host %s: %d, want %d", test.host, response.Code, test.want)
		}
	}
}
