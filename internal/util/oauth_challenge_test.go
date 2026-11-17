package util

import (
 "crypto/tls"
 "errors"
 "net/http"
 "strings"
 "testing"
)

func TestOAuthChallenge(t *testing.T) {
 cases := []struct { name, auth, errText string; status int; required, forbidden string }{
  {"missing", "", "verify OAuth Authorization header invalid", 401, `Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
  {"other-scheme", "Basic abc", "verify OAuth Authorization header invalid", 401, `Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
  {"bearer-no-token", "Bearer", "verify OAuth Authorization header invalid", 401, `Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
  {"bearer-prefix", "BearerXYZ abc", "verify OAuth Authorization header invalid", 401, `Bearer realm="http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1"`, "error="},
  {"invalid", "Bearer invalid", "verify OAuth parse: malformed", 401, `error="invalid_token"`, "insufficient_scope"},
  {"scope", "Bearer valid-but-other-scope", scopeVerificationFailure, 403, `error="insufficient_scope", scope="npcf-smpolicycontrol"`, "invalid_token"},
  {"mismatch", "Bearer valid", scopeVerificationFailure+" extra", 401, `error="invalid_token"`, "insufficient_scope"},
 }
 for _, tc := range cases {
  t.Run(tc.name, func(t *testing.T) {
   req, err := http.NewRequest("GET", "http://pcf.free5gc.org:8000/npcf-smpolicycontrol/v1/sm-policies/unknown", nil)
   if err != nil { t.Fatal(err) }
   status, header, _ := oauthChallenge(req, "npcf-smpolicycontrol", tc.auth, errors.New(tc.errText))
   if status != tc.status || !strings.Contains(header,tc.required) || strings.Contains(header,tc.forbidden) {
    t.Fatalf("status=%d challenge=%q, want status=%d required=%s forbidden=%s",status,header,tc.status,tc.required,tc.forbidden)
   }
  })
 }
}

func TestOAuthChallengeHTTPS(t *testing.T) {
 req, err := http.NewRequest("GET","https://udr.free5gc.org:8000/nudr-dr/v2/lookup",nil)
 if err != nil { t.Fatal(err) }
 req.TLS=&tls.ConnectionState{}
 status, header, _ := oauthChallenge(req,"nudr-dr","Bearer invalid",errors.New("invalid"))
 if status!=401 || !strings.Contains(header,`realm="https://udr.free5gc.org:8000/nudr-dr/v2"`) {t.Fatalf("status=%d header=%q",status,header)}
 status,header,reason := oauthChallenge(req,"nudr-dr","",nil)
 if status!=0 || header!="" || reason!="" { t.Fatal("unexpected response for nil error") }
}
