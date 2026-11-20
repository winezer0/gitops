package gitops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitSSH "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	sshknownhosts "golang.org/x/crypto/ssh/knownhosts"
)

const gitOpsSSHProtocol = "gitops-ssh"

func init() {
	transportclient.InstallProtocol(gitOpsSSHProtocol, goGitSSHTransport{})
}

type goGitSSHTransport struct{}

func (goGitSSHTransport) NewUploadPackSession(endpoint *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	if endpoint.Proxy.URL != "" {
		return nil, ErrUnsupported
	}
	sshAuth, ok := auth.(*goGitSSHAuth)
	if !ok {
		return nil, ErrCredentialNotConfigured
	}
	return &goGitSSHSession{endpoint: endpoint, auth: sshAuth}, nil
}

func (goGitSSHTransport) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return nil, ErrUnsupported
}

type goGitSSHAuth struct {
	clientConfig *ssh.ClientConfig
	timeout      time.Duration
}

func (auth *goGitSSHAuth) Name() string   { return "gitops-public-keys" }
func (auth *goGitSSHAuth) String() string { return "gitops SSH public key authentication" }

type goGitSSHSession struct {
	mu       sync.Mutex
	endpoint *transport.Endpoint
	auth     *goGitSSHAuth
	conn     net.Conn
	client   *ssh.Client
	session  *ssh.Session
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	refs     *packp.AdvRefs
}

func (session *goGitSSHSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return session.AdvertisedReferencesContext(context.Background())
}

func (session *goGitSSHSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.refs != nil {
		return session.refs, nil
	}
	if err := session.start(ctx); err != nil {
		return nil, err
	}
	connection := session.conn
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()
	refs := packp.NewAdvRefs()
	if err := refs.Decode(session.stdout); err != nil {
		return nil, fmt.Errorf("read SSH advertised references: %w", err)
	}
	session.refs = refs
	return refs, nil
}

func (session *goGitSSHSession) UploadPack(ctx context.Context, request *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.refs == nil {
		return nil, errors.New("SSH references must be read before requesting a pack")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection := session.conn
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()
	if err := request.UploadRequest.Encode(session.stdin); err != nil {
		return nil, fmt.Errorf("write SSH upload-pack request: %w", err)
	}
	if err := request.UploadHaves.Encode(session.stdin, true); err != nil {
		return nil, fmt.Errorf("write SSH upload-pack haves: %w", err)
	}
	if len(request.UploadHaves.Haves) != 0 {
		return nil, ErrUnsupported
	}
	if err := pktline.NewEncoder(session.stdin).Encodef("done\n"); err != nil {
		return nil, fmt.Errorf("finish SSH upload-pack negotiation: %w", err)
	}
	if err := session.stdin.Close(); err != nil {
		return nil, fmt.Errorf("finish SSH upload-pack request: %w", err)
	}
	response := packp.NewUploadPackResponse(request)
	if err := response.Decode(io.NopCloser(session.stdout)); err != nil {
		return nil, fmt.Errorf("read SSH upload-pack response: %w", err)
	}
	return response, nil
}

func (session *goGitSSHSession) start(ctx context.Context) error {
	if session.client != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	port := session.endpoint.Port
	if port <= 0 {
		port = gitSSH.DefaultPort
	}
	address := net.JoinHostPort(session.endpoint.Host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: session.auth.timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	session.conn = connection
	deadline := time.Now().Add(session.auth.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = connection.SetDeadline(deadline)
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	clientConn, channels, requests, err := ssh.NewClientConn(connection, address, session.auth.clientConfig)
	stopCancel()
	if err != nil {
		_ = connection.Close()
		return classifySSHHostKeyError(err)
	}
	session.client = ssh.NewClient(clientConn, channels, requests)
	sshSession, err := session.client.NewSession()
	if err != nil {
		_ = session.client.Close()
		return err
	}
	session.session = sshSession
	session.stdin, err = sshSession.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := sshSession.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := sshSession.StderrPipe()
	if err != nil {
		return err
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	if err := sshSession.Start("git-upload-pack " + quoteRemotePath(session.endpoint.Path)); err != nil {
		return err
	}
	session.stdout = bufio.NewReader(stdout)
	return nil
}

func (session *goGitSSHSession) Close() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.session != nil {
		_ = session.session.Close()
		session.session = nil
	}
	if session.client != nil {
		_ = session.client.Close()
		session.client = nil
	}
	if session.conn != nil {
		_ = session.conn.Close()
		session.conn = nil
	}
	return nil
}

func quoteRemotePath(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (config Config) sshHostKeyCallback() (ssh.HostKeyCallback, error) {
	callback := config.SSHHostKeyCallback
	if callback == nil {
		var err error
		switch {
		case len(config.SSHKnownHostsFiles) > 0:
			callback, err = gitSSH.NewKnownHostsCallback(config.SSHKnownHostsFiles...)
		case config.UseUserSSHKnownHosts:
			callback, err = gitSSH.NewKnownHostsCallback()
		default:
			return nil, ErrSSHHostKeyTrustNotConfigured
		}
		if err != nil {
			return nil, fmt.Errorf("load SSH known_hosts: %w", err)
		}
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		if err := callback(host, remote, key); err != nil {
			switch {
			case knownhosts.IsHostUnknown(err):
				return fmt.Errorf("%w: %v", ErrSSHHostKeyNotTrusted, err)
			case knownhosts.IsHostKeyChanged(err):
				return fmt.Errorf("%w: %v", ErrSSHHostKeyChanged, err)
			default:
				return err
			}
		}
		return nil
	}, nil
}

func classifySSHHostKeyError(err error) error {
	var keyError *sshknownhosts.KeyError
	if errors.As(err, &keyError) {
		if len(keyError.Want) == 0 {
			return fmt.Errorf("%w: %v", ErrSSHHostKeyNotTrusted, err)
		}
		return fmt.Errorf("%w: %v", ErrSSHHostKeyChanged, err)
	}
	return err
}
