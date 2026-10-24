package sbi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	nfctx "github.com/free5gc/nef/internal/context"
	"github.com/free5gc/nef/pkg/factory"
	"github.com/free5gc/openapi"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/openapi/oauth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/oauth2"
)

func TestLiveOAuthAcceptance(t *testing.T) {
	endpoint := os.Getenv("OAUTH_NRF_URL")
	if endpoint == "" {
		t.Skip("run from NRF acceptance harness")
	}
	ids := map[string]string{}
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("OAUTH_IDS")), &ids))
	producer := ids["NEF"]
	cert := os.Getenv("OAUTH_NRF_CERT")
	app := &callbackOAuthApp{cfg: &factory.Config{Configuration: &factory.Configuration{NfInstanceId: producer, NrfUri: endpoint, NrfCertPem: cert, Sbi: &factory.Sbi{BindingIPv4: "127.0.0.1"}}}}
	ctx, err := nfctx.NewContext(app)
	require.NoError(t, err)
	app.ctx = ctx
	require.NoError(t, ctx.SetOAuth2Required(true))
	client, err := mongo.Connect(t.Context(), options.Client().ApplyURI(os.Getenv("OAUTH_MONGO_URI")))
	require.NoError(t, err)
	defer client.Disconnect(context.Background())
	collection := client.Database(os.Getenv("OAUTH_DB")).Collection("NfProfile")
	services := []models.Nrf_NFMgmt_ServiceName{models.Nrf_NFMgmt_ServiceName_3GPP_TRAFFIC_INFLUENCE, models.Nrf_NFMgmt_ServiceName_NNEF_PFDMANAGEMENT, nfctx.ServiceNameNnefCallback}
	documents := bson.A{}
	for _, service := range services {
		allowed, known := nfctx.AllowedNfTypesForService(service)
		require.True(t, known)
		documents = append(documents, bson.M{"serviceInstanceId": uuid.NewString(), "serviceName": string(service), "nfServiceStatus": "REGISTERED", "allowedNfTypes": allowed})
	}
	profile := map[string]interface{}{"nfInstanceId": producer, "nfType": "NEF", "nfStatus": "REGISTERED", "nfServices": documents}
	profileJSON, err := json.Marshal(profile)
	require.NoError(t, err)
	registerURL := endpoint + "/nnrf-nfm/v1/nf-instances/" + producer
	registerReq, err := http.NewRequestWithContext(t.Context(), http.MethodPut, registerURL, bytes.NewReader(profileJSON))
	require.NoError(t, err)
	registerReq.Header.Set("Content-Type", "application/json")
	registered, err := http.DefaultClient.Do(registerReq)
	require.NoError(t, err)
	registerBody, err := io.ReadAll(registered.Body)
	require.NoError(t, err)
	registered.Body.Close()
	require.Equal(t, http.StatusCreated, registered.StatusCode, string(registerBody))
	require.Equal(t, registerURL, registered.Header.Get("Location"))
	var stored bson.M
	require.NoError(t, collection.FindOne(t.Context(), bson.M{"nfInstanceId": producer}).Decode(&stored))
	storedJSON, err := json.Marshal(stored["nfServices"])
	require.NoError(t, err)
	var savedServices []models.Nrf_NFMgmt_NFService
	require.NoError(t, json.Unmarshal(storedJSON, &savedServices))
	require.Len(t, savedServices, len(services))
	for i, service := range services {
		allowed, _ := nfctx.AllowedNfTypesForService(service)
		require.Equal(t, service, savedServices[i].ServiceName)
		require.Equal(t, allowed, savedServices[i].AllowedNfTypes)
	}

	t.Log("registration: 201, exact Location and persisted service policies verified")

	// This exercises the NF's own token helper and the real NRF issuer identity.
	nrfTokenCtx, pd, err := ctx.GetTokenCtxForNRF(models.Nrf_NFMgmt_ServiceName_NNRF_DISC)
	require.NoError(t, err)
	require.Nil(t, pd)
	tokenString := func(c context.Context) string {
		t.Helper()
		ts, ok := c.Value(openapi.ContextOAuth2).(oauth2.TokenSource)
		require.True(t, ok)
		token, e := ts.Token()
		require.NoError(t, e)
		return token.AccessToken
	}
	require.NoError(t, oauth.VerifyOAuth("Bearer "+tokenString(nrfTokenCtx), "nnrf-disc", oauth.AudiencePolicy{NFInstanceID: os.Getenv("OAUTH_NRF_ID")}, os.Getenv("OAUTH_NRF_ID"), cert))
	discoveryURL := endpoint + "/nnrf-disc/v1/nf-instances?target-nf-type=NEF&requester-nf-type=NEF"
	for _, authorized := range []bool{false, true} {
		req, e := http.NewRequestWithContext(t.Context(), http.MethodGet, discoveryURL, nil)
		require.NoError(t, e)
		if authorized {
			req.Header.Set("Authorization", "Bearer "+tokenString(nrfTokenCtx))
		}
		resp, e := http.DefaultClient.Do(req)
		require.NoError(t, e)
		body, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		resp.Body.Close()
		if !authorized {
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
			continue
		}
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var found models.Nrf_NFDisc_SearchResult
		require.NoError(t, json.Unmarshal(body, &found))
		matched := false
		for _, profile := range found.NfInstances {
			if profile.NfInstanceId == producer {
				matched = true
			}
		}
		require.True(t, matched, "registered producer must be discoverable")
	}
	t.Log("discovery: 401 without token, 200 and registered producer with NRF token")
	// Use actual NRF HTTP responses for denied requests, avoiding token caching.
	rejected := func(requester, scope string) {
		t.Helper()
		form := url.Values{"grant_type": {"client_credentials"}, "nfInstanceId": {ids[requester]}, "nfType": {requester}, "targetNfType": {"NEF"}, "targetNfInstanceId": {producer}, "scope": {scope}}
		req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+"/oauth2/token", strings.NewReader(form.Encode()))
		require.NoError(t, e)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, e := http.DefaultClient.Do(req)
		require.NoError(t, e)
		defer resp.Body.Close()
		body, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &decoded))
		require.Equal(t, "invalid_scope", decoded["error"], string(body))
		require.NotContains(t, decoded, "access_token")
	}
	server, e := NewServer(app, "")
	require.NoError(t, e)
	callbackServer := httptest.NewServer(server.router)
	defer callbackServer.Close()
	callback := func(token string, want int) {
		t.Helper()
		req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, callbackServer.URL+factory.NefCallbackResUriPrefix+"/notification/smf", strings.NewReader("{"))
		require.NoError(t, e)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, e := http.DefaultClient.Do(req)
		require.NoError(t, e)
		defer resp.Body.Close()
		body, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		require.Equal(t, want, resp.StatusCode, string(body))
	}
	callback("", http.StatusUnauthorized)
	for _, service := range services {
		t.Run(string(service), func(t *testing.T) {
			allowed, _ := nfctx.AllowedNfTypesForService(service)
			require.NotEmpty(t, allowed)
			requester := string(allowed[0])
			require.NotEmpty(t, ids[requester])
			for _, target := range []string{"", producer} {
				t.Run(map[bool]string{true: "type", false: "instance"}[target == ""], func(t *testing.T) {
					request := oauth.TokenRequest{ConsumerNFType: allowed[0], ConsumerNFInstanceID: ids[requester], TargetNFType: models.Nrf_NFMgmt_NFType_NEF, TargetNFInstanceID: target, NRFURI: endpoint, Scope: string(service)}
					tokenCtx, problem, e := oauth.GetTokenCtx(request)
					require.NoError(t, e)
					require.Nil(t, problem)
					token := tokenString(tokenCtx)
					require.NoError(t, ctx.AuthorizationCheck("Bearer "+token, service))
					parsed, e := jwt.Parse(token, func(_ *jwt.Token) (interface{}, error) { return oauth.ParsePublicKeyFromPEM(cert) }, jwt.WithValidMethods([]string{"RS512"}), jwt.WithExpirationRequired(), jwt.WithIssuer(os.Getenv("OAUTH_NRF_ID")))
					require.NoError(t, e)
					claims := parsed.Claims.(jwt.MapClaims)
					subject, e := claims.GetSubject()
					require.NoError(t, e)
					require.Equal(t, ids[requester], subject)
					aud, e := claims.GetAudience()
					require.NoError(t, e)
					wantAudience := target
					if target == "" {
						wantAudience = "NEF"
					}
					require.Equal(t, jwt.ClaimStrings{wantAudience}, aud)
					require.Equal(t, string(service), claims["scope"])
					if string(service) == "nnef-callback" {
						callback(token, http.StatusBadRequest)
					} else {
						callback(token, http.StatusUnauthorized)
					}
				})
			}
			t.Run("denied_nf_type", func(t *testing.T) { rejected("AUSF", string(service)) })
			t.Run("unknown_scope", func(t *testing.T) { rejected(requester, "unknown-service") })
			t.Run("mixed_scope", func(t *testing.T) { rejected(requester, string(service)+" unknown-service") })
		})
	}
}
