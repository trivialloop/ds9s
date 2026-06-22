package dockerx

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"ds9s/internal/config"
)

// sshDialer holds an established SSH connection to the manager (optionally
// reached through a proxy-jump host). Every call to Dial opens a fresh SSH
// "exec" session that runs the configured remote command (by default
// `docker system dial-stdio`, optionally through sudo) and exposes its
// stdin/stdout as a net.Conn, exactly like the docker CLI's own ssh://
// transport. This is what lets `sudo` apply at all: a raw unix-socket
// forward happens at the SSH protocol level and is bound by the *login*
// user's permissions, with no opportunity to interpose sudo; running an
// actual remote command is the only way to do that.
type sshDialer struct {
	client  *ssh.Client
	command string
}

// newSSHDialer dials the configured SSH hop(s) and returns a dialer that
// runs cfg's remote command (see buildRemoteCommand) for every connection
// the docker client needs.
func newSSHDialer(cfg config.SSHConfig) (*sshDialer, error) {
	authMethods, err := sshAuthMethods(cfg)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := sshHostKeyCallback(cfg.KnownHosts)
	if err != nil {
		return nil, err
	}

	clientConfig := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	target := cfg.Addr
	command := buildRemoteCommand(cfg)

	if cfg.ProxyJump != "" {
		jumpClient, err := ssh.Dial("tcp", cfg.ProxyJump, clientConfig)
		if err != nil {
			return nil, fmt.Errorf("dialing ssh proxy-jump %s: %w", cfg.ProxyJump, err)
		}
		jumpConn, err := jumpClient.Dial("tcp", target)
		if err != nil {
			return nil, fmt.Errorf("ssh proxy-jump %s -> %s: %w", cfg.ProxyJump, target, err)
		}
		sshConn, chans, reqs, err := ssh.NewClientConn(jumpConn, target, clientConfig)
		if err != nil {
			return nil, fmt.Errorf("ssh handshake via proxy-jump to %s: %w", target, err)
		}
		client := ssh.NewClient(sshConn, chans, reqs)
		return &sshDialer{client: client, command: command}, nil
	}

	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dialing ssh host %s: %w", target, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, target, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh handshake with %s: %w", target, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	return &sshDialer{client: client, command: command}, nil
}

// buildRemoteCommand decides what to run on the manager to reach the docker
// API. An explicit cfg.Command always wins; otherwise it's
// `docker system dial-stdio`, optionally pointed at a non-default socket via
// DOCKER_HOST, optionally prefixed with `sudo -n`.
func buildRemoteCommand(cfg config.SSHConfig) string {
	if cfg.Command != "" {
		return cfg.Command
	}

	cmd := "docker system dial-stdio"
	if cfg.RemoteSocket != "" {
		cmd = fmt.Sprintf("env DOCKER_HOST=unix://%s %s", cfg.RemoteSocket, cmd)
	}
	if cfg.Sudo {
		// -n: fail fast instead of hanging on an interactive password prompt
		// ds9s has no way to answer. Requires NOPASSWD sudo rights for the
		// remote user, see config.go's doc comment.
		cmd = "sudo -n " + cmd
	}
	return cmd
}

// Dial matches the signature used by client.WithDialContext: network/addr as
// requested by the docker HTTP client are ignored, since every "connection"
// is really a fresh SSH exec session running the configured command.
func (d *sshDialer) Dial(network, addr string) (net.Conn, error) {
	session, err := d.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("opening ssh session: %w", err)
	}
	conn, err := newSSHExecConn(session, d.command)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("running %q over ssh: %w", d.command, err)
	}
	return conn, nil
}

func (d *sshDialer) Close() error {
	if d.client != nil {
		return d.client.Close()
	}
	return nil
}

// sshExecConn adapts an SSH session's stdin/stdout into a net.Conn so it can
// be plugged into an http.Transport's DialContext. It also tracks the
// session's exit status and captured stderr so that a remote-command failure
// (e.g. "sudo: a password is required") surfaces as a readable error instead
// of a bare EOF.
type sshExecConn struct {
	session *ssh.Session
	stdout  interface {
		Read(p []byte) (int, error)
	}
	stdinW interface {
		Write(p []byte) (int, error)
		Close() error
	}

	mu      sync.Mutex
	waitErr error
	stderr  *bytes.Buffer
	done    chan struct{}
}

func newSSHExecConn(session *ssh.Session, command string) (*sshExecConn, error) {
	stdin, err := session.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	session.Stderr = &stderrBuf

	if err := session.Start(command); err != nil {
		return nil, fmt.Errorf("starting remote command: %w", err)
	}

	c := &sshExecConn{
		session: session,
		stdout:  stdout,
		stdinW:  stdin,
		stderr:  &stderrBuf,
		done:    make(chan struct{}),
	}
	go func() {
		err := session.Wait()
		c.mu.Lock()
		c.waitErr = err
		c.mu.Unlock()
		close(c.done)
	}()
	return c, nil
}

func (c *sshExecConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if err != nil {
		if detail := c.failureDetail(); detail != "" {
			return n, fmt.Errorf("%w: %s", err, detail)
		}
	}
	return n, err
}

// failureDetail returns the remote command's stderr output if it has
// already exited (successfully or not). Used to turn an otherwise opaque
// EOF into something actionable, e.g. a sudo or "command not found" error.
func (c *sshExecConn) failureDetail() string {
	select {
	case <-c.done:
	default:
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := strings.TrimSpace(c.stderr.String())
	switch {
	case msg != "":
		return msg
	case c.waitErr != nil:
		return c.waitErr.Error()
	default:
		return ""
	}
}

func (c *sshExecConn) Write(p []byte) (int, error) { return c.stdinW.Write(p) }

func (c *sshExecConn) Close() error {
	_ = c.stdinW.Close()
	return c.session.Close()
}

func (c *sshExecConn) LocalAddr() net.Addr                { return sshAddr{} }
func (c *sshExecConn) RemoteAddr() net.Addr               { return sshAddr{} }
func (c *sshExecConn) SetDeadline(t time.Time) error      { return nil }
func (c *sshExecConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *sshExecConn) SetWriteDeadline(t time.Time) error { return nil }

type sshAddr struct{}

func (sshAddr) Network() string { return "ssh" }
func (sshAddr) String() string  { return "ssh-exec-tunnel" }

func sshAuthMethods(cfg config.SSHConfig) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if cfg.PrivateKey != "" {
		keyPath := expandHome(cfg.PrivateKey)
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("reading ssh private key %s: %w", keyPath, err)
		}
		var signer ssh.Signer
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(keyBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("parsing ssh private key %s: %w", keyPath, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("ssh config for %s has neither privateKey nor password set", cfg.Addr)
	}
	return methods, nil
}

func sshHostKeyCallback(knownHostsPath string) (ssh.HostKeyCallback, error) {
	if knownHostsPath == "" {
		// Explicit opt-out: no host key file configured means we don't verify.
		// #nosec G106 -- user-controlled trade-off, documented in config.go
		return ssh.InsecureIgnoreHostKey(), nil
	}
	path := expandHome(knownHostsPath)
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("loading known_hosts %s: %w", path, err)
	}
	return cb, nil
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath_join(home, strings.TrimPrefix(p, "~"))
}

// filepath_join avoids importing path/filepath solely for this one call site
// while still handling the leading slash left after trimming "~".
func filepath_join(home, rest string) string {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return home
	}
	return home + string(os.PathSeparator) + rest
}
