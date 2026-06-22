// Package config handles loading and resolving ds9s configuration files.
//
// Config layout (YAML), default path: $XDG_CONFIG_HOME/ds9s/config.yaml
// (falls back to ~/.config/ds9s/config.yaml):
//
//	current: prod-manager1
//	refreshRate: 2
//	managers:
//	  - name: prod-manager1
//	    host: unix:///var/run/docker.sock
//
//	  - name: prod-manager2
//	    host: tcp://10.0.0.5:2376
//	    tls:
//	      ca: /path/ca.pem
//	      cert: /path/cert.pem
//	      key: /path/key.pem
//
//	  - name: remote-via-ssh
//	    ssh:
//	      addr: bastion.example.com:22
//	      user: deploy
//	      privateKey: ~/.ssh/id_rsa
//	      # password: "..."             # alternative to privateKey
//	      # proxyJump: jump.example.com:22  # optional extra hop before addr
//	      # knownHosts: ~/.ssh/known_hosts  # if empty, host key checking is skipped (insecure)
//	      # sudo: true                  # if the ssh user isn't in the `docker` group
//	      # remoteSocket: /var/run/docker.sock  # only if the daemon uses a non-default socket
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// TLSConfig holds client TLS material for talking to a TCP docker daemon.
type TLSConfig struct {
	CA                 string `yaml:"ca,omitempty"`
	Cert               string `yaml:"cert,omitempty"`
	Key                string `yaml:"key,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty"`
}

// SSHConfig describes how to reach a manager's docker daemon through an SSH hop.
//
// ds9s never forwards the unix socket directly: a raw socket forward happens
// at the SSH protocol level and is bound by the *login* user's permissions,
// with no way to interpose sudo. Instead it runs `docker system dial-stdio`
// as a remote command over the SSH session (the same trick the docker CLI
// itself uses for ssh:// hosts), which means Sudo below actually works.
//
// If Sudo is true, the remote user needs passwordless sudo rights for the
// docker command, since ds9s cannot answer an interactive password prompt,
// e.g. in /etc/sudoers on the manager:
//
//	deploy ALL=(root) NOPASSWD: /usr/bin/docker
type SSHConfig struct {
	Addr       string `yaml:"addr"` // host:port of the ssh server (the manager, or the entry bastion)
	User       string `yaml:"user"`
	Password   string `yaml:"password,omitempty"`
	PrivateKey string `yaml:"privateKey,omitempty"` // path to a private key file
	Passphrase string `yaml:"passphrase,omitempty"` // passphrase for the private key, if any
	ProxyJump  string `yaml:"proxyJump,omitempty"`  // optional host:port of a jump host, reached with the same user/auth
	KnownHosts string `yaml:"knownHosts,omitempty"` // path to known_hosts; empty disables host key verification

	// Sudo runs the remote docker command through `sudo -n` when the SSH
	// user isn't in the `docker` group on the manager. Requires NOPASSWD
	// sudo rights for that command (see doc comment above).
	Sudo bool `yaml:"sudo,omitempty"`

	// RemoteSocket overrides the docker socket path used on the remote host
	// (passed as DOCKER_HOST). Leave empty to use the manager's default
	// (/var/run/docker.sock).
	RemoteSocket string `yaml:"remoteSocket,omitempty"`

	// Command fully overrides the command ds9s runs over SSH to reach the
	// docker API (default: "docker system dial-stdio", wrapped with sudo
	// and/or DOCKER_HOST as configured above). Most setups don't need this.
	Command string `yaml:"command,omitempty"`
}

// Manager describes a single Swarm manager endpoint ds9s can connect to.
type Manager struct {
	Name string     `yaml:"name"`
	Host string     `yaml:"host"` // docker host URL: unix:///..., tcp://host:port
	TLS  *TLSConfig `yaml:"tls,omitempty"`
	SSH  *SSHConfig `yaml:"ssh,omitempty"`
}

// Config is the root ds9s configuration.
type Config struct {
	Current     string    `yaml:"current"`
	RefreshRate int       `yaml:"refreshRate"` // seconds, default 2
	Managers    []Manager `yaml:"managers"`
}

// DefaultPath returns the conventional config file location.
func DefaultPath() string {
	if dir := os.Getenv("DS9S_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.yaml")
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ds9s", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "ds9s", "config.yaml")
}

// Load reads and parses the config file at path. If path is empty, DefaultPath() is used.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if cfg.RefreshRate <= 0 {
		cfg.RefreshRate = 2
	}
	if len(cfg.Managers) == 0 {
		return nil, fmt.Errorf("config %s defines no managers", path)
	}
	return &cfg, nil
}

// ManagerByName returns the manager with the given name, or the first manager
// if name is empty and Current is unset.
func (c *Config) ManagerByName(name string) (*Manager, error) {
	if name == "" {
		name = c.Current
	}
	if name == "" {
		m := c.Managers[0]
		return &m, nil
	}
	for i := range c.Managers {
		if c.Managers[i].Name == name {
			m := c.Managers[i]
			return &m, nil
		}
	}
	return nil, fmt.Errorf("no manager named %q in config", name)
}

// Names returns the list of configured manager names, in order.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Managers))
	for _, m := range c.Managers {
		names = append(names, m.Name)
	}
	return names
}
