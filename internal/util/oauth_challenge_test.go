package util

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestOAuthChallenge(t *testing.T) {
	tests := []struct {
		name string
		auth string
		errText string
		status int
		want string
		forbidden string
	}{
		{"missing", "", "verify OAuth Authorization header invalid", http.StatusUnauthorized,
			`Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
		{"other-scheme", "Basic abc", "verify OAuth Authorization header invalid", http.StatusUnauthorized,
			`Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
		{"bearer-no-token", "Bearer", "verify OAuth Authorization header invalid", http.StatusUnauthorized,
			`Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
		{"bearer-prefix", "BearerXYZ abc", "verify OAuth Authorization header invalid", http.StatusUnauthorized,
			`Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
		{"invalid", "Bearer invalid", "verify OAuth parse: malformed", http.StatusUnauthorized,
			`error="invalid_token"`, "insufficient_scope"},
		{"scope", "Bearer valid-but-other-scope", scopeVerificationFailure, http.StatusForbidden,
			`error="insufficient_scope", scope="npcf-smpolicycontrol"`, "invalid_token"},
		{"partial-wording-failclosed", "Bearer valid", scopeVerificationFailure + " extra", http.StatusUnauthorized,
			`error="invalid_token"`, "insufficient_scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				"http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1/sm-policies/unknown", nil)
			if err != nil {
				t.Fatal(err)
			}
			status, header, _ := oauthChallenge(r, "npcf-smpolicycontrol", tc.auth, errors.New(tc.errText),
				"http://pcf.free5gc.org:8000")
			if status != tc.status {
				t.Fatalf("status: got %d want %d", status, tc.status)
			}
			if !strings.Contains(header, tc.want) {
				t.Fatalf("header %q lacks %q", header, tc.want)
			}
			if strings.Contains(header, tc.forbidden) {
				t.Fatalf("header %q contains %q", header, tc.forbidden)
			}
		})
	}
}

func TestChallengeIgnoresAttackerHost(t *testing.T) {
	r, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet,
		"http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1/invalid", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "attacker.example"
	_, header, _ := oauthChallenge(r, "npcf-smpolicycontrol", "", errors.New("missing"),
		"http://pcf.free5gc.org:8000")
	if strings.Contains(header, "attacker.example") || !strings.Contains(header, "pcf.free5gc.org:8000") {
		t.Fatalf("unsafe realm: %q", header)
	}
}

func TestOAuthChallengeNilAndHTTPS(t *testing.T) {
	r, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://udr.free5gc.org:8000/nudr-dr/v2/lookup", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	r.TLS = &tls.ConnectionState{}
	status, header, _ := oauthChallenge(r, "nudr-dr", "Bearer bad", errors.New("invalid"), "https://udr.free5gc.org:8000")
	if status != 401 || !strings.Contains(header, `realm="https://udr.free5gc.org:8000/nudr-dr/v2"`) {
		t.Fatalf("HTTPS/version realm: status=%d header=%q", status, header)
	}
	status, header, reason := oauthChallenge(r, "nudr-dr", "", nil, "https://udr.free5gc.org:8000")
	if status != 0 || header != "" || reason != "" {
		t.Fatalf("unexpected response on nil error")
	}
}
