package gitops

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHHostCredentialAuthenticatesWithoutInteractiveShell(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	_, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userPrivate)
	if err != nil {
		t.Fatal(err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(userPrivate, "test")
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(privateBlock)

	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), userSigner.PublicKey().Marshal()) {
			return nil, errors.New("unexpected user key")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		_, channels, requests, handshakeErr := ssh.NewServerConn(connection, serverConfig)
		if handshakeErr != nil {
			serverDone <- handshakeErr
			return
		}
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			_ = channel.Reject(ssh.Prohibited, "interactive sessions are disabled")
		}
		serverDone <- nil
	}()

	client, err := NewClient(Config{
		AllowedHosts:       []string{listener.Addr().String()},
		SSHHostKeyCallback: ssh.FixedHostKey(hostSigner.PublicKey()),
		Timeout:            5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.TestHostCredential(context.Background(), CredentialTestRequest{
		Host: listener.Addr().String(),
		Credential: &Credential{
			Username:      "git",
			SSHPrivateKey: privateKey,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CredentialStatus != CredentialStatusAccepted || result.ProfileAccess != ProfileAccessNotChecked {
		t.Fatalf("SSH credential status = (%s, %s): %#v", result.CredentialStatus, result.ProfileAccess, result.Diagnostic)
	}
	select {
	case serverErr := <-serverDone:
		if serverErr != nil {
			t.Fatalf("SSH test server failed: %v", serverErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSH server did not finish after client authentication")
	}
}

func TestGoGitSSHProbeAndCloneUseInProcessTransport(t *testing.T) {
	bare := createBareFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	_, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userPrivate)
	if err != nil {
		t.Fatal(err)
	}
	userBlock, err := ssh.MarshalPrivateKey(userPrivate, "test")
	if err != nil {
		t.Fatal(err)
	}
	credential := Credential{Username: "git", SSHPrivateKey: pem.EncodeToMemory(userBlock)}
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), userSigner.PublicKey().Marshal()) {
			return nil, errors.New("unexpected user key")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	go serveGitUploadPackOverSSH(listener, serverConfig, bare)

	client, err := NewClient(Config{
		AllowedHosts:       []string{listener.Addr().String()},
		SSHHostKeyCallback: ssh.FixedHostKey(hostSigner.PublicKey()),
		Timeout:            10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	repositoryURL := fmt.Sprintf("ssh://git@%s/repo.git", listener.Addr().String())
	probe, err := client.Probe(context.Background(), ProbeRequest{URL: repositoryURL, Credential: &credential})
	if err != nil {
		var operationError *OperationError
		_ = errors.As(err, &operationError)
		t.Fatalf("SSH probe failed: %v; cause=%T %v", err, operationError.Cause, operationError.Cause)
	}
	if probe.DefaultBranch != "main" || probe.ResolvedCommit == "" {
		t.Fatalf("SSH probe returned unexpected refs: %#v", probe)
	}
	destination := filepath.Join(t.TempDir(), "checkout")
	clone, err := client.Clone(context.Background(), CloneRequest{
		URL: repositoryURL, Ref: "main", Destination: destination, Credential: &credential,
	})
	if err != nil {
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			t.Logf("SSH clone error: %T %v", cause, cause)
		}
		t.Fatalf("SSH clone failed: %v", err)
	}
	if clone.Commit != probe.ResolvedCommit {
		t.Fatalf("SSH clone commit = %s, probe commit = %s", clone.Commit, probe.ResolvedCommit)
	}
	if _, err := os.Stat(filepath.Join(destination, "README.md")); err != nil {
		t.Fatalf("SSH clone did not materialize README: %v", err)
	}
	valid, _, err := client.ValidateCheckout(destination, repositoryURL, clone.Commit)
	if err != nil || !valid {
		t.Fatalf("SSH clone origin was not restored to its public URL: valid=%t err=%v", valid, err)
	}
}

func serveGitUploadPackOverSSH(listener net.Listener, config *ssh.ServerConfig, repositoryPath string) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer connection.Close()
			_, channels, requests, err := ssh.NewServerConn(connection, config)
			if err != nil {
				return
			}
			go ssh.DiscardRequests(requests)
			for incoming := range channels {
				channel, channelRequests, err := incoming.Accept()
				if err != nil {
					continue
				}
				go func() {
					defer channel.Close()
					for request := range channelRequests {
						if request.Type != "exec" {
							_ = request.Reply(false, nil)
							continue
						}
						var payload struct{ Command string }
						if err := ssh.Unmarshal(request.Payload, &payload); err != nil || !strings.HasPrefix(payload.Command, "git-upload-pack ") {
							_ = request.Reply(false, nil)
							return
						}
						_ = request.Reply(true, nil)
						command := exec.Command("git", "upload-pack", repositoryPath)
						command.Stdin = channel
						command.Stdout = channel
						command.Stderr = channel.Stderr()
						err := command.Run()
						status := uint32(0)
						if err != nil {
							status = 1
						}
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: status}))
						return
					}
				}()
			}
		}()
	}
}
