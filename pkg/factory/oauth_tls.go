package factory

import (
	"net/http"

	"github.com/free5gc/openapi"
	"github.com/free5gc/openapi/oauth"
)

// OAuthTLS supplies the NF client identity and the CA bundle trusted for the
// NRF token endpoint. It is independent of the NRF JWT signing certificate.
type OAuthTLS struct {
	Cert string `yaml:"cert" valid:"required"`
	Key  string `yaml:"key" valid:"required"`
	CA   string `yaml:"ca" valid:"required"`
}

// NewOAuthHTTPClient returns nil for the existing default transport when no
// OAuth TLS configuration is supplied. Invalid explicit configuration is fatal.
func (c *Config) NewOAuthHTTPClient() (*http.Client, error) {
	if c.Configuration == nil || c.Configuration.OAuthTLS == nil {
		return nil, nil
	}
	cfg := c.Configuration.OAuthTLS
	return oauth.NewMTLSClient(cfg.Cert, cfg.Key, cfg.CA,
		openapi.ClientTimeout, c.GetReadIdleTimeout(), c.GetPingTimeout())
}
