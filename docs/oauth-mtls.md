# OAuth token client mTLS

For an NRF token endpoint requiring mutual TLS, configure this NF before startup:

```yaml
configuration:
  oauthTls:
    cert: ./cert/chf-client.pem
    key: ./cert/chf-client.key
    ca: ./cert/nrf-server-ca.pem
```

The certificate and key must match. The client certificate chain must be trusted
by the NRF. The CA bundle must validate the NRF TLS server certificate, including
the hostname/IP in `nrfUri`. This CA bundle is separate from `nrfCertPem`, which
verifies the NRF's JWT signing identity. No verification is disabled.

When this optional block is absent, the existing default token transport remains.
Invalid explicit certificate/key/CA configuration fails startup. The new client
uses the existing client timeout and configured HTTP/2 read-idle/ping timeouts.
Configure once at startup; restart to rotate certificates. Token/client caches
are isolated by HTTP client pointer, so clients must be reused without mutation.

Scope: token acquisition through the NF context. This does not configure the
separate NRF registration/discovery consumer transports. Whole-process HTTPS
registration and discovery require corresponding transport configuration and
are not claimed by the token-client acceptance test.

The HTTP/2 POST token request and `/oauth2/token` path remain unchanged (3GPP
TS 29.510 V17.7.0, sections 6.3.2, 6.3.3.1 and 6.3.4.2.2). Explicit client
configuration and cache keys are implementation choices.
