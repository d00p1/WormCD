// Package cluster manages connections to Docker Engine endpoints.
package cluster

import (
	"fmt"
	"net/url"

	"github.com/moby/moby/client"
)

// Config describes a Docker Engine endpoint registered in WormCD.
type Config struct {
	// Name is the cluster name used in manifests.
	Name string
	// Endpoint is a Docker Engine endpoint: unix:// or tcp://.
	Endpoint string
	// CAFile, CertFile and KeyFile configure mTLS for tcp:// endpoints.
	CAFile   string
	CertFile string
	KeyFile  string
}

// New returns a Docker API client for cfg. It does not contact the daemon:
// the first API call negotiates the API version.
func New(cfg Config) (*client.Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("cluster %q: endpoint is empty", cfg.Name)
	}

	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("cluster %q: parse endpoint: %w", cfg.Name, err)
	}

	opts := []client.Opt{client.WithHost(cfg.Endpoint)}

	switch u.Scheme {
	case "unix":
		if cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
			return nil, fmt.Errorf("cluster %q: TLS is not supported for unix endpoints", cfg.Name)
		}
	case "tcp":
		if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, fmt.Errorf("cluster %q: tcp endpoint requires CA, cert and key", cfg.Name)
		}
		opts = append(opts, client.WithTLSClientConfig(cfg.CAFile, cfg.CertFile, cfg.KeyFile))
	default:
		return nil, fmt.Errorf("cluster %q: unsupported endpoint scheme %q (want unix or tcp)", cfg.Name, u.Scheme)
	}

	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("cluster %q: create client: %w", cfg.Name, err)
	}
	return cli, nil
}
