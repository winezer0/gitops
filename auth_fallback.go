package gitops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sshFallbackTransport = "https->ssh"

func (client *Client) shouldFallbackSSH(repository Repository, credential Credential, err error) bool {
	return client.config.FallbackSSHOnHTTPSAuthFailure &&
		repository.Transport == "https" &&
		credential.Token == "" && credential.Password == "" &&
		errorKind(err) == ErrorAuthentication
}

func (client *Client) prepareSSHFallback(ctx context.Context, repository Repository) (Repository, Credential, error) {
	sshURL, err := sshFallbackURL(repository)
	if err != nil {
		return Repository{}, Credential{}, err
	}
	fallbackClient := &Client{config: client.config}
	if resolve := client.config.CredentialForRepository; resolve != nil {
		fallbackClient.config.CredentialForRepository = func(repository Repository) (Credential, error) {
			credential, err := resolve(repository)
			if err != nil {
				return Credential{}, err
			}
			return sshCredentialOnly(credential)
		}
	}
	if resolve := client.config.CredentialFor; resolve != nil {
		fallbackClient.config.CredentialFor = func(host string) (Credential, error) {
			credential, err := resolve(host)
			if err != nil {
				return Credential{}, err
			}
			return sshCredentialOnly(credential)
		}
	}
	return fallbackClient.prepare(ctx, sshURL)
}

func sshCredentialOnly(credential Credential) (Credential, error) {
	if len(credential.SSHPrivateKey) == 0 && credential.SSHPrivateKeyPath == "" {
		return Credential{}, ErrCredentialNotConfigured
	}
	return Credential{
		Username:          credential.Username,
		SSHPrivateKey:     credential.SSHPrivateKey,
		SSHPrivateKeyPath: credential.SSHPrivateKeyPath,
		Passphrase:        credential.Passphrase,
		Provider:          credential.Provider,
	}, nil
}

func sshFallbackURL(repository Repository) (string, error) {
	if repository.Transport != "https" || repository.Host == "" || repository.Path == "" {
		return "", ErrInvalidURL
	}
	parsed, err := url.Parse(repository.URL)
	if err != nil || parsed.Hostname() == "" {
		return "", ErrInvalidURL
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "22")
	}
	return (&url.URL{Scheme: "ssh", User: url.User("git"), Host: host, Path: parsed.Path}).String(), nil
}

func fallbackFailure(firstErr, fallbackErr error, repository Repository, ref string, timeout, duration time.Duration) error {
	diagnostic := diagnosticForError(fallbackErr, "ssh_fallback", repository, ref, timeout, duration)
	diagnostic.Code = "git_transport_fallback_failed"
	diagnostic.Transport = sshFallbackTransport
	diagnostic.Message = "HTTPS authentication was rejected; the SSH fallback also failed. " + diagnostic.Message
	diagnostic.Recommendation = "Check SSH credentials and host-key trust, or configure a valid HTTPS token for this Git host."
	return &OperationError{Kind: errorKind(fallbackErr), Retryable: diagnostic.Retryable, Cause: errors.Join(firstErr, fallbackErr), Diagnostic: &diagnostic}
}

func sshCredentialSource(config Config, credential Credential) string {
	if len(credential.SSHPrivateKey) > 0 || credential.SSHPrivateKeyPath != "" {
		return "configured SSH credential"
	}
	if config.DiscoverSSHKeys {
		return "local SSH config and key discovery"
	}
	return "configured SSH credential"
}

func fallbackSuccessDiagnostic(repository Repository, ref string, timeout, duration time.Duration, credentialSource string) Diagnostic {
	return Diagnostic{
		Code: "git_transport_fallback_succeeded", Phase: "ssh_fallback",
		Message:        "HTTPS authentication was rejected; repository access succeeded over SSH.",
		Recommendation: "SSH credential source: " + credentialSource + ".",
		Host:           limitedDiagnosticText(repository.Host, 255),
		Ref:            limitedDiagnosticText(ref, 256),
		Transport:      sshFallbackTransport,
		Duration:       duration,
		Timeout:        timeout,
	}
}

func clearStagingDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func restoreWorkspaceOrigin(path string, repository Repository) error {
	workingCopy, err := goGitOpen(path)
	if err != nil {
		return fmt.Errorf("open fallback checkout: %w", err)
	}
	configuration, err := workingCopy.Config()
	if err != nil {
		return fmt.Errorf("read fallback checkout configuration: %w", err)
	}
	origin := configuration.Remotes["origin"]
	if origin == nil || len(origin.URLs) == 0 {
		return fmt.Errorf("fallback checkout has no origin remote")
	}
	origin.URLs = []string{repository.URL}
	if err := workingCopy.SetConfig(configuration); err != nil {
		return fmt.Errorf("restore fallback checkout origin: %w", err)
	}
	return nil
}
