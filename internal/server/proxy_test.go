package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tronbyt-server/internal/config"

	"github.com/stretchr/testify/assert"
)

func TestProxyMiddlewareTrustBoundary(t *testing.T) {
	s := &Server{Config: &config.Settings{TrustedProxies: "10.0.0.0/8"}}
	handler := s.ProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(s.GetBaseURL(r)))
	}))

	untrusted := httptest.NewRequest(http.MethodGet, "http://internal:8000/", nil)
	untrusted.RemoteAddr = "203.0.113.9:4321"
	untrusted.Header.Set("X-Forwarded-Proto", "https")
	untrusted.Header.Set("X-Forwarded-Host", "display.example.com")
	untrustedResponse := httptest.NewRecorder()
	handler.ServeHTTP(untrustedResponse, untrusted)
	assert.Equal(t, "http://internal:8000", untrustedResponse.Body.String())

	trusted := httptest.NewRequest(http.MethodGet, "http://server:8000/", nil)
	trusted.RemoteAddr = "10.0.0.2:4321"
	trusted.Header.Set("X-Forwarded-Proto", "https")
	trusted.Header.Set("X-Forwarded-Host", "display.example.com")
	trusted.Header.Set("X-Forwarded-Port", "443")
	trustedResponse := httptest.NewRecorder()
	handler.ServeHTTP(trustedResponse, trusted)
	assert.Equal(t, "https://display.example.com:443", trustedResponse.Body.String())
}
