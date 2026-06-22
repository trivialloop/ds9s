// Package dockerx wraps the Docker Engine SDK client construction so the
// rest of ds9s only deals with a ready-to-use *client.Client, regardless of
// whether the manager is reached over a local socket, TCP+TLS, or through an
// SSH hop (with an optional proxy-jump).
package dockerx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/docker/docker/client"

	"ds9s/internal/config"
)

// Connection wraps a docker client plus anything that must be torn down
// (e.g. an SSH session) when the manager is no longer in use.
type Connection struct {
	Manager config.Manager
	Client  *client.Client

	closeFns []func() error
}

// Close releases the underlying transport (SSH session, idle TCP conns...).
func (c *Connection) Close() error {
	var first error
	for _, fn := range c.closeFns {
		if err := fn(); err != nil && first == nil {
			first = err
		}
	}
	if c.Client != nil {
		if err := c.Client.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Connect builds a docker client for the given manager configuration.
func Connect(m config.Manager) (*Connection, error) {
	if m.SSH != nil {
		return connectViaSSH(m)
	}
	return connectDirect(m)
}

func connectDirect(m config.Manager) (*Connection, error) {
	opts := []client.Opt{
		client.WithHost(m.Host),
		client.WithAPIVersionNegotiation(),
	}

	if m.TLS != nil {
		tlsConfig, err := buildTLSConfig(*m.TLS)
		if err != nil {
			return nil, err
		}
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		}
		opts = append(opts, client.WithHTTPClient(httpClient))
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating docker client for %s: %w", m.Name, err)
	}
	return &Connection{Manager: m, Client: cli}, nil
}

func connectViaSSH(m config.Manager) (*Connection, error) {
	dialer, err := newSSHDialer(*m.SSH)
	if err != nil {
		return nil, fmt.Errorf("manager %s: %w", m.Name, err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
			IdleConnTimeout: 30 * time.Second,
		},
	}

	cli, err := client.NewClientWithOpts(
		// The docker SDK still needs *a* host value to build request URLs;
		// it is never actually dialed since DialContext is overridden above.
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithHTTPClient(httpClient),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		_ = dialer.Close()
		return nil, fmt.Errorf("creating docker client for %s: %w", m.Name, err)
	}

	return &Connection{
		Manager:  m,
		Client:   cli,
		closeFns: []func() error{dialer.Close},
	}, nil
}

func buildTLSConfig(t config.TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: t.InsecureSkipVerify, // #nosec G402 -- explicit opt-in via config
	}

	if t.CA != "" {
		caBytes, err := os.ReadFile(t.CA)
		if err != nil {
			return nil, fmt.Errorf("reading CA %s: %w", t.CA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("no certificates found in CA file %s", t.CA)
		}
		tlsConfig.RootCAs = pool
	}

	if t.Cert != "" && t.Key != "" {
		cert, err := tls.LoadX509KeyPair(t.Cert, t.Key)
		if err != nil {
			return nil, fmt.Errorf("loading client cert/key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}
