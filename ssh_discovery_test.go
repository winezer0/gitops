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
	if len(discovered.IdentityFiles) != 2 {
		t.Fatalf("SSH identity candidates = %#v, want 2", discovered.IdentityFiles)
	}
	for i, want := range []string{keyOne, keyTwo} {
		gotInfo, err := os.Stat(discovered.IdentityFiles[i])
		if err != nil {
			t.Fatalf("stat discovered SSH identity %q: %v", discovered.IdentityFiles[i], err)
		}
		wantInfo, err := os.Stat(want)
		if err != nil {
			t.Fatalf("stat expected SSH identity %q: %v", want, err)
		}
		if !os.SameFile(gotInfo, wantInfo) {
			t.Errorf("SSH identity candidate %q does not identify expected file %q", discovered.IdentityFiles[i], want)
		}
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

func TestResolveSSHIdentityPathAllowsSymlinkedHome(t *testing.T) {
	realHome := t.TempDir()
	identityPath := createSSHIdentityFile(t, filepath.Join(realHome, "id_ed25519"))
	linkParent := t.TempDir()
	homeAlias := filepath.Join(linkParent, "home")
	if err := os.Symlink(realHome, homeAlias); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}

	resolved, err := resolveSSHIdentityPath(filepath.Join(homeAlias, "id_ed25519"), homeAlias, "alias", "host", "user", "22")
	if err != nil {
		t.Fatal(err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	identityInfo, err := os.Stat(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(resolvedInfo, identityInfo) {
		t.Fatalf("resolved identity = %q, want file %q", resolved, identityPath)
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
	if err := os.WriteFile(filepath.Join(sshDirectory, "config"), []byte("Host *\n ProxyJump bastion\n"), 0o600); err != nil {
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

func TestSSHDiscoverySupportsIncludeAndMatchHostUser(t *testing.T) {
	_, sshDirectory := prepareSSHHome(t)
	if err := os.WriteFile(filepath.Join(sshDirectory, "config"), []byte("Include included.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "included.conf"), []byte("Match host git-alias user git\n HostName matched.example.test\n Port 2222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := ParseURL("ssh://git-alias/team/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	got, err := discoverSSHConfig(repo, Config{DiscoverSSHKeys: true, SSHConfigDir: sshDirectory})
	if err != nil {
		t.Fatal(err)
	}
	if got.HostName != "matched.example.test" || got.Port != "2222" {
		t.Fatalf("discovered config = %#v", got)
	}
}

func TestSSHConfigPatternListsSupportNegation(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		value    string
		want     bool
	}{
		{[]string{"*.example.test", "!blocked.example.test"}, "git.example.test", true},
		{[]string{"*.example.test", "!blocked.example.test"}, "blocked.example.test", false},
		{[]string{"!blocked.example.test"}, "git.example.test", false},
	} {
		if got := sshConfigPatternListMatches(tc.patterns, tc.value); got != tc.want {
			t.Errorf("match %v against %q = %v, want %v", tc.patterns, tc.value, got, tc.want)
		}
	}
}

func TestSSHDiscoveryMatchThenHostStartsNewSection(t *testing.T) {
	_, dir := prepareSSHHome(t)
	contents := "Match host does-not-match\n HostName ignored.example.test\nHost git-alias\n HostName selected.example.test\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := ParseURL("ssh://git-alias/team/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	got, err := discoverSSHConfig(repo, Config{DiscoverSSHKeys: true, SSHConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got.HostName != "selected.example.test" {
		t.Fatalf("HostName = %q", got.HostName)
	}
}

func TestSSHConfigIncludeRejectsCycleOutsideHomeAndFileLimit(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config")
	if err := os.WriteFile(config, []byte("Include config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSSHConfig(config, home, "host", "user", 0, map[string]bool{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("cycle error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("Host *\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSSHConfig(outside, home, "host", "user", 0, map[string]bool{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("outside path error = %v", err)
	}
	limits := &sshConfigLoadLimits{files: maxSSHConfigFiles}
	if _, err := loadSSHConfigLimited(config, home, "host", "user", 0, map[string]bool{}, limits); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("file limit error = %v", err)
	}
}

func TestSSHConfigIncludeGlobOrderIsStable(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"02.conf": "HostName second.example.test\n", "01.conf": "HostName first.example.test\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	main := filepath.Join(dir, "config")
	if err := os.WriteFile(main, []byte("Include *.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadSSHConfig(main, home, "host", "user", 0, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(got, "first.example.test") > strings.Index(got, "second.example.test") {
		t.Fatalf("include order = %q", got)
	}
}

func TestSSHDiscoverySupportsQuotedIncludeAndMatchOriginalHostLocalUser(t *testing.T) {
	_, sshDirectory := prepareSSHHome(t)
	localUser, err := userpkgCurrent()
	if err != nil {
		t.Fatal(err)
	}
	contents := fmt.Sprintf("Include \"with space.conf\"\n")
	if err := os.WriteFile(filepath.Join(sshDirectory, "config"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	child := fmt.Sprintf("Match originalhost git-alias localuser \"%s\"\n HostName quoted.example.test\n", localUser)
	if err := os.WriteFile(filepath.Join(sshDirectory, "with space.conf"), []byte(child), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := ParseURL("ssh://git-alias/team/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSSHConfig(filepath.Join(sshDirectory, "config"), filepath.Dir(sshDirectory), "git-alias", "git", 0, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded, "HostName quoted.example.test") {
		t.Fatalf("loaded config = %q", loaded)
	}
	got, err := discoverSSHConfig(repo, Config{DiscoverSSHKeys: true, SSHConfigDir: sshDirectory})
	if err != nil {
		t.Fatal(err)
	}
	if got.HostName != "quoted.example.test" {
		t.Fatalf("HostName = %q", got.HostName)
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
