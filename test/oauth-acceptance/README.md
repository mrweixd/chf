# OAuth 4.4 integration acceptance

This harness uses a real MongoDB 8.0.16 process, the NRF #100 production HTTP router and token processor, and the actual CHF/PCF/NEF context and callback routing code. Tests and child binaries are built with Go's race detector. Every repository is pinned in `run.sh`.

Run on Ubuntu 24.04 with Go 1.26.2, Git, curl and tar:

```sh
bash test/oauth-acceptance/run.sh
```

The script clones disposable checkouts, adds acceptance-only tests, downloads MongoDB if `OAUTH_MONGOD` is unset, and deletes the checkouts afterward. MongoDB listens only on loopback; a new database and temporary test CA/certificates are used. No production database or secrets are needed.

## Coverage

- The NF's own `GetTokenCtxForNRF` requests and obtains a token from the real NRF.
- Producer profiles use the actual `AllowedNfTypesForService` functions. The three producer profiles are registered through the real NRF PUT endpoint; 201, exact Location, and stored service allowlists are checked. Other caller fixtures are seeded directly.
- For six services (CHF charging, PCF SM policy and callback, NEF traffic influence, PFD management and callback), a permitted NF obtains type-level and instance-level tokens; the actual producer context verifies them.
- Signature, RS512, issuer, subject, audience, scope and required expiration are checked on NRF-issued tokens.
- An AUSF outside each allowlist is denied with HTTP 400 `invalid_scope`, with no access token. Unknown scopes and a mixed allowed/unknown scope are also denied.
- PCF and NEF production callback routes receive real HTTP requests: missing or wrong-scope tokens return 401; callback-scoped NRF tokens reach the JSON parser (400 for deliberately malformed JSON).

## HTTPS/mTLS acceptance

The harness also verifies discovery (401 without a token; 200 containing the newly registered producer with an NRF-issued token). The HTTPS/mTLS suite requires each NF's actual `GetTokenCtxForNRF` path, with a factory-loaded client certificate/key/CA bundle, to obtain a token from the real NRF and use it for a 200 discovery response. Repeating the same token request with the default client (no client certificate) must fail even after successful token acquisition; cached credentials cannot hide the missing certificate.

## Boundaries

Producer registration and discovery use real NRF endpoints, but full NF application startup, deregistration and deployment provisioning are not acceptance claims. The mTLS discovery HTTP request uses the configured token client explicitly; the separate NF registration/discovery consumer clients are not configured by this change. Most token tests use local cleartext HTTP/2; a separate HTTPS/mTLS suite checks all three NF token clients. Successful callback business processing is not tested: 400 after authentication is intentional. No complete core/UE session is deployed. One allowed caller per service and AUSF as the denied caller are tested, not every caller combination.

NRF #100 is tested with its OpenAPI replacement changed only in the disposable checkout to `github.com/mrweixd/openapi v0.0.0-20261025094827-bd904d23aed6`, matching all three NF forks. This is not an update to the original NRF PR.

Protocol reference: 3GPP TS 29.510 V17.7.0, sections 6.3.5.2.2 (AccessTokenReq) and 6.3.5.2.4 (AccessTokenClaims). The private callback scope names are existing project conventions; these tests do not claim the specification defines those names.

A test run is accepted only if all child suites and the parent suite pass with no race report. Logs are stored under `results/` and uploaded by the workflow even on failure.
