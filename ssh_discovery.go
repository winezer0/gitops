package gitops

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
)

const defaultMaxSSHIdentityFiles = 8

type discoveredSSHConfig struct {
	Alias         string
	HostName      string
	User          string
	Port          string
	IdentityFiles []string
}

func discoverSSHConfig(repository Repository, config Config) (discoveredSSHConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return discoveredSSHConfig{}, fmt.Errorf("resolve user home directory for SSH discovery: %w", err)
	}
	sshDirectory := config.SSHConfigDir
	if sshDirectory == "" {
		sshDirectory = filepath.Join(home, ".ssh")
	} else {
		sshDirectory = expandSSHConfigHome(sshDirectory, home)
	}
	if !filepath.IsAbs(sshDirectory) {
		sshDirectory = filepath.Join(home, sshDirectory)
	}
	sshDirectory, err = filepath.Abs(sshDirectory)
	if err != nil {
		return discoveredSSHConfig{}, err
	}
	alias, explicitPort, err := sshAliasAndPort(repository.URL)
	if err != nil {
		return discoveredSSHConfig{}, err
	}
	result := discoveredSSHConfig{Alias: alias, Port: explicitPort}
	configPath := filepath.Join(sshDirectory, "config")
	file, err := os.Open(configPath)
	if errors.Is(err, os.ErrNotExist) {
		result.IdentityFiles = defaultSSHIdentityFiles(home, sshDirectory)
		result.IdentityFiles, err = limitSSHIdentityFiles(result, config)
		return result, err
	}
	if err != nil {
		return discoveredSSHConfig{}, fmt.Errorf("read SSH config: %w", err)
	}
	defer file.Close()
	if err := rejectUnsupportedSSHDirectives(file); err != nil {
		return discoveredSSHConfig{}, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return discoveredSSHConfig{}, err
	}
	parsed, err := ssh_config.Decode(file)
	if err != nil {
		return discoveredSSHConfig{}, fmt.Errorf("parse SSH config: %w", err)
	}
	result.HostName, err = parsed.Get(alias, "HostName")
	if err != nil {
		return discoveredSSHConfig{}, fmt.Errorf("resolve SSH HostName: %w", err)
	}
	result.User, err = parsed.Get(alias, "User")
	if err != nil {
		return discoveredSSHConfig{}, fmt.Errorf("resolve SSH User: %w", err)
	}
	if explicitPort == "" {
		result.Port, err = parsed.Get(alias, "Port")
		if err != nil {
			return discoveredSSHConfig{}, fmt.Errorf("resolve SSH Port: %w", err)
		}
	}
	if result.Port != "" {
		port, parseErr := strconv.Atoi(result.Port)
		if parseErr != nil || port < 1 || port > 65535 {
			return discoveredSSHConfig{}, errors.New("SSH config contains an invalid port")
		}
	}
	identityValues, err := parsed.GetAll(alias, "IdentityFile")
	if err != nil {
		return discoveredSSHConfig{}, fmt.Errorf("resolve SSH IdentityFile: %w", err)
	}
	if len(identityValues) == 0 {
		result.IdentityFiles = defaultSSHIdentityFiles(home, sshDirectory)
	} else {
		result.IdentityFiles = make([]string, 0, len(identityValues))
		for _, value := range identityValues {
			identityPath, err := resolveSSHIdentityPath(value, home, alias, result.HostName, result.User, result.Port)
			if err != nil {
				return discoveredSSHConfig{}, err
			}
			result.IdentityFiles = append(result.IdentityFiles, identityPath)
		}
	}
	result.IdentityFiles, err = limitSSHIdentityFiles(result, config)
	if err != nil {
		return discoveredSSHConfig{}, err
	}
	return result, nil
}

func sshAliasAndPort(raw string) (string, string, error) {
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			return "", "", ErrInvalidURL
		}
		return parsed.Hostname(), parsed.Port(), nil
	}
	left, _, found := strings.Cut(raw, ":")
	if !found {
		return "", "", ErrInvalidURL
	}
	_, host, hasUser := strings.Cut(left, "@")
	if !hasUser || host == "" {
		return "", "", ErrInvalidURL
	}
	return host, "", nil
}

func rejectUnsupportedSSHDirectives(file *os.File) error {
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.ReplaceAll(line, "=", " ")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "include", "match", "proxyjump", "proxycommand":
			return fmt.Errorf("%w: SSH config directive %s", ErrUnsupported, fields[0])
		}
	}
	return scanner.Err()
}

func resolveSSHIdentityPath(value, home, alias, hostName, user, port string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "none") {
		return "", ErrCredentialNotConfigured
	}
	value = strings.Trim(value, "\"'")
	value = strings.ReplaceAll(value, "%d", home)
	value = strings.ReplaceAll(value, "%h", firstNonEmpty(hostName, alias))
	value = strings.ReplaceAll(value, "%r", user)
	value = strings.ReplaceAll(value, "%p", port)
	if strings.Contains(value, "%") {
		return "", fmt.Errorf("%w: unsupported SSH IdentityFile token", ErrUnsupported)
	}
	value = expandSSHConfigHome(value, home)
	if !filepath.IsAbs(value) {
		value = filepath.Join(home, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if err := withinRoot(home, absolute); err != nil {
		return "", fmt.Errorf("%w: SSH IdentityFile is outside the user home directory", ErrUnsupported)
	}
	// Resolve both roots before comparing so Windows junctions do not look like escapes.
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve SSH user home directory", ErrUnsupported)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		if err := withinRoot(resolvedHome, resolved); err != nil {
			return "", fmt.Errorf("%w: SSH IdentityFile symlink leaves the user home directory", ErrUnsupported)
		}
		absolute = resolved
	}
	return absolute, nil
}

func expandSSHConfigHome(value, home string) string {
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		return filepath.Join(home, value[2:])
	}
	return value
}

func defaultSSHIdentityFiles(home, sshDirectory string) []string {
	base := sshDirectory
	if base == "" {
		base = filepath.Join(home, ".ssh")
	}
	return []string{
		filepath.Join(base, "id_ed25519"),
		filepath.Join(base, "id_ecdsa"),
		filepath.Join(base, "id_rsa"),
	}
}

func limitSSHIdentityFiles(discovered discoveredSSHConfig, config Config) ([]string, error) {
	limit := config.MaxSSHIdentityFiles
	if limit <= 0 {
		limit = defaultMaxSSHIdentityFiles
	}
	paths := discovered.IdentityFiles
	if len(paths) > limit {
		return nil, ErrSSHIdentityLimit
	}
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		key := filepath.Clean(path)
		if os.PathSeparator == '\\' {
			key = strings.ToLower(key)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, path)
	}
	return unique, nil
}

func loadSSHIdentitySigners(paths []string, config Config) ([]ssh.Signer, error) {
	var signers []ssh.Signer
	var candidateError error
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			candidateError = errors.Join(candidateError, fmt.Errorf("read discovered SSH identity: %w", err))
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		var missingPassphrase *ssh.PassphraseMissingError
		if errors.As(err, &missingPassphrase) {
			if config.SSHKeyPassphraseFor == nil {
				candidateError = errors.Join(candidateError, ErrSSHKeyPassphraseMissing)
				continue
			}
			passphrase, passErr := config.SSHKeyPassphraseFor(path)
			if passErr != nil {
				candidateError = errors.Join(candidateError, passErr)
				continue
			}
			if passphrase == "" {
				candidateError = errors.Join(candidateError, ErrSSHKeyPassphraseMissing)
				continue
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		}
		if err != nil {
			candidateError = errors.Join(candidateError, fmt.Errorf("invalid discovered SSH identity: %w", err))
			continue
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		return signers, nil
	}
	if candidateError != nil {
		return nil, candidateError
	}
	return nil, ErrCredentialNotConfigured
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
