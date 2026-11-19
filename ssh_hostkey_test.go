package gitops

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestUnknownSSHHostKeyIsRejectedBeforeAuthentication(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system Git is required to exercise the SSH transport")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is required to exercise host-key validation")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("failed to start the local SSH test server")
	}
	defer listener.Close()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("failed to generate the local SSH host key")
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal("failed to prepare the local SSH host key")
	}
	var authenticationAttempts atomic.Int32
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		authenticationAttempts.Add(1)
		return nil, errors.New("test server rejects user authentication")
	}}
	serverConfig.AddHostKey(signer)
	serverResult := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer connection.Close()
		_, channels, requests, handshakeErr := ssh.NewServerConn(connection, serverConfig)
		if handshakeErr != nil {
			serverResult <- handshakeErr
			return
		}
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			_ = channel.Reject(ssh.Prohibited, "no Git service is configured")
		}
		serverResult <- nil
	}()

	home := t.TempDir()
	sshDirectory := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatal("failed to prepare the isolated SSH home")
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "config"), nil, 0o600); err != nil {
		t.Fatal("failed to prepare isolated SSH configuration")
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "known_hosts"), nil, 0o600); err != nil {
		t.Fatal("failed to prepare empty known-hosts file")
	}
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", filepath.VolumeName(home))
		t.Setenv("HOMEPATH", strings.TrimPrefix(home, filepath.VolumeName(home)))
	}

	client, err := NewClient(Config{
		AllowedHosts: []string{listener.Addr().String()},
		Timeout:      10 * time.Second,
	})
	if err != nil {
		t.Fatal("failed to create SSH test client")
	}
	url := "ssh://git@" + listener.Addr().String() + "/team/private.git"
	_, probeErr := client.Probe(context.Background(), ProbeRequest{URL: url})
	if ErrorCode(probeErr) != string(ErrorAuthentication) {
		t.Fatalf("unknown-host-key error code = %s, want %s", ErrorCode(probeErr), ErrorAuthentication)
	}
	select {
	case handshakeErr := <-serverResult:
		if handshakeErr == nil {
			t.Fatal("SSH handshake succeeded despite an empty known-hosts file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local SSH server did not observe the host-key exchange")
	}
	if attempts := authenticationAttempts.Load(); attempts != 0 {
		t.Fatalf("server received %d user-authentication attempts before host-key approval", attempts)
	}
}
