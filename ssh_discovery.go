package gitops

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
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
	file.Close()
	contents, err := loadSSHConfig(configPath, home, alias, firstNonEmpty(repositorySSHUser(repository.URL), "git"), 0, map[string]bool{})
	if err != nil {
		return discoveredSSHConfig{}, err
	}
	parsed, err := ssh_config.Decode(strings.NewReader(contents))
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

const maxSSHConfigIncludeDepth = 8
const maxSSHConfigFiles = 64
const maxSSHConfigBytes = 1 << 20

type sshConfigLoadLimits struct {
	files int
	bytes int
}

func loadSSHConfig(path, home, host, user string, depth int, active map[string]bool) (string, error) {
	limits := &sshConfigLoadLimits{}
	return loadSSHConfigLimited(path, home, host, user, depth, active, limits)
}

func loadSSHConfigLimited(path, home, host, user string, depth int, active map[string]bool, limits *sshConfigLoadLimits) (string, error) {
	if depth > maxSSHConfigIncludeDepth {
		return "", fmt.Errorf("%w: SSH config Include depth exceeded", ErrUnsupported)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if err := withinRoot(home, abs); err != nil {
		return "", fmt.Errorf("%w: SSH config Include is outside user home", ErrUnsupported)
	}
	resolvedHome, rootErr := filepath.EvalSymlinks(home)
	resolvedPath, pathErr := filepath.EvalSymlinks(abs)
	if rootErr != nil || pathErr != nil || withinRoot(resolvedHome, resolvedPath) != nil {
		return "", fmt.Errorf("%w: SSH config Include resolves outside user home", ErrUnsupported)
	}
	if active[resolvedPath] {
		return "", fmt.Errorf("%w: SSH config Include cycle", ErrUnsupported)
	}
	active[resolvedPath] = true
	defer delete(active, resolvedPath)
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read SSH config: %w", err)
	}
	limits.files++
	limits.bytes += len(b)
	if limits.files > maxSSHConfigFiles || limits.bytes > maxSSHConfigBytes {
		return "", fmt.Errorf("%w: SSH config Include limits exceeded", ErrUnsupported)
	}
	var out strings.Builder
	enabled := true
	s := bufio.NewScanner(strings.NewReader(string(b)))
	s.Buffer(make([]byte, 4096), maxSSHConfigBytes)
	for s.Scan() {
		line := s.Text()
		trimmed := strings.TrimSpace(line)
		fields, tokenErr := sshConfigFields(trimmed)
		if tokenErr != nil {
			return "", tokenErr
		}
		if len(fields) == 0 || strings.HasPrefix(trimmed, "#") {
			if enabled {
				out.WriteString(line + "\n")
			}
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "host":
			// Host begins a new section and must remain visible to ssh_config.Decode.
			// A preceding Match condition no longer gates this Host section.
			out.WriteString(line + "\n")
			enabled = true
		case "include":
			if len(fields) < 2 {
				return "", fmt.Errorf("invalid SSH config Include")
			}
			if enabled {
				for _, pattern := range fields[1:] {
					p := expandSSHConfigHome(strings.Trim(pattern, "\"'"), home)
					if !filepath.IsAbs(p) {
						p = filepath.Join(filepath.Dir(abs), p)
					}
					matches, e := filepath.Glob(p)
					if e != nil {
						return "", e
					}
					for _, m := range matches {
						nested, e := loadSSHConfigLimited(m, home, host, user, depth+1, active, limits)
						if e != nil {
							return "", e
						}
						out.WriteString(nested)
					}
				}
			}
		case "match":
			if len(fields) < 3 {
				return "", fmt.Errorf("invalid SSH config Match")
			}
			ok := true
			i := 1
			for i < len(fields) {
				key := strings.ToLower(fields[i])
				if key == "all" {
					i++
					continue
				}
				if i+1 >= len(fields) {
					return "", fmt.Errorf("invalid SSH config Match")
				}
				vals := strings.Split(fields[i+1], ",")
				target := host
				switch key {
				case "user":
					target = user
				case "originalhost":
					target = host
				case "localuser":
					current, e := userpkgCurrent()
					if e != nil {
						return "", e
					}
					target = current
				case "host":
				default:
					return "", fmt.Errorf("%w: SSH config Match %s", ErrUnsupported, key)
				}
				matched := sshConfigPatternListMatches(vals, target)
				if !matched {
					ok = false
				}
				i += 2
			}
			enabled = ok
		default:
			if strings.EqualFold(fields[0], "proxyjump") || strings.EqualFold(fields[0], "proxycommand") {
				return "", fmt.Errorf("%w: SSH config directive %s", ErrUnsupported, fields[0])
			}
			if enabled {
				out.WriteString(line + "\n")
			}
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// SSH config pattern lists are ORed; a matching negative pattern vetoes the list.
func sshConfigPatternListMatches(patterns []string, value string) bool {
	positive := false
	for _, pattern := range patterns {
		for _, item := range strings.Split(pattern, ",") {
			negative := strings.HasPrefix(item, "!")
			item = strings.TrimPrefix(item, "!")
			matched, err := filepath.Match(strings.ToLower(item), strings.ToLower(value))
			if err != nil {
				continue
			}
			if matched && negative {
				return false
			}
			if matched {
				positive = true
			}
		}
	}
	return positive
}

func userpkgCurrent() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.Username, nil
}

func sshConfigFields(line string) ([]string, error) {
	var fields []string
	var b strings.Builder
	quote := rune(0)
	escaped := false
	started := false
	for _, r := range line {
		if escaped {
			if r != ' ' && r != '\t' && r != '#' && r != '\\' && r != '"' && r != '\'' {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			// Preserve Windows path/user separators; only consume backslashes that
			// escape syntax characters in a config token.
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == '#' && !started {
			break
		}
		if r == '=' {
			if started {
				fields = append(fields, b.String())
				b.Reset()
				started = false
			}
			continue
		}
		if r == ' ' || r == '\t' {
			if started {
				fields = append(fields, b.String())
				b.Reset()
				started = false
			}
			continue
		}
		b.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("invalid quoting in SSH config")
	}
	if started {
		fields = append(fields, b.String())
	}
	return fields, nil
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
