package gitops

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestLocalSSHDiscoveryResolvesAliasAndMultipleIdentityFiles(t *testing.T) {
	home, sshDirectory := prepareSSHHome(t)
	keyOne := createSSHIdentityFile(t, filepath.Join(sshDirectory, "id_one"))
	keyTwo := createSSHIdentityFile(t, filepath.Join(sshDirectory, "id_two"))
	configPath := filepath.Join(sshDirectory, "config")
	contents := fmt.Sprintf("Host git-alias\n HostName code.example.test\n User deploy\n Port 2222\n IdentityFile %s\n IdentityFile %s\n", filepath.Join(sshDirectory, "id_one"), filepath.Join(sshDirectory, "id_two"))
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := ParseURL("ssh://git-alias/team/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		DiscoverSSHKeys: true,
		SSHConfigDir:    sshDirectory,
		SSHHostKeyCallback: ssh.FixedHostKey(func() ssh.PublicKey {
			_, privateKey, keyErr := ed25519.GenerateKey(rand.Reader)
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			signer, signerErr := ssh.NewSignerFromKey(privateKey)
			if signerErr != nil {
				t.Fatal(signerErr)
			}
			return signer.PublicKey()
		}()),
	}
	discovered, err := discoverSSHConfig(repository, config)
	if err != nil {
		t.Fatal(err)
	}
	if discovered.HostName != "code.example.test" || discovered.User != "deploy" || discovered.Port != "2222" {
		t.Fatalf("SSH alias configuration = %#v", discovered)
	}
	if len(discovered.IdentityFiles) != 2 || discovered.IdentityFiles[0] != keyOne || discovered.IdentityFiles[1] != keyTwo {
		t.Fatalf("SSH identity candidates = %#v", discovered.IdentityFiles)
	}
	signers, err := loadSSHIdentitySigners(discovered.IdentityFiles, config)
	if err != nil || len(signers) != 2 {
		t.Fatalf("loaded SSH signers = %d, err=%v", len(signers), err)
	}
	remoteURL, err := goGitRemoteURL(repository, config, Credential{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(remoteURL, "gitops-ssh://deploy@code.example.test:2222/") {
		t.Fatalf("resolved SSH transport URL = %q", remoteURL)
	}
	if home == "" {
		t.Fatal("test SSH home directory was not set")
	}
}

func TestSSHDiscoveryIsOptInAndRevalidatesAliasTarget(t *testing.T) {
	home, sshDirectory := prepareSSHHome(t)
	if err := os.WriteFile(filepath.Join(sshDirectory, "config"), []byte("Host git-alias\n HostName target.example.test\n IdentityFile ~/.ssh/id_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := ParseURL("ssh://git-alias/team/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authMethod(repository, Credential{}, Config{}, time.Second); !errors.Is(err, ErrCredentialNotConfigured) {
		t.Fatalf("SSH discovery should be disabled by default, got %v", err)
	}
	client, err := NewClient(Config{
		AllowedHosts:    []string{"git-alias"},
		DiscoverSSHKeys: true,
		SSHConfigDir:    sshDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.prepare(t.Context(), repository.URL); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("SSH alias target should be revalidated, got %v", err)
	}
	if home == "" {
		t.Fatal("test SSH home directory was not set")
	}
}

func TestSSHDiscoveryRejectsUnsupportedRoutingDirectives(t *testing.T) {
	_, sshDirectory := prepareSSHHome(t)
	if err := os.WriteFile(filepath.Join(sshDirectory, "config"), []byte("Host *\n Include other.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := ParseURL("ssh://git.example.test/team/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	_, err = discoverSSHConfig(repository, Config{DiscoverSSHKeys: true, SSHConfigDir: sshDirectory})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported SSH routing directive error = %v", err)
	}
}

func prepareSSHHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	sshDirectory := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if runtime.GOOS == "windows" {
		t.Setenv("HOMEDRIVE", filepath.VolumeName(home))
		t.Setenv("HOMEPATH", strings.TrimPrefix(home, filepath.VolumeName(home)))
	}
	return home, sshDirectory
}

func createSSHIdentityFile(t *testing.T, path string) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
