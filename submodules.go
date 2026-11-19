package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const maxSubmoduleDepth = 32

type submoduleCheckout struct {
	Path   string
	URL    string
	Commit string
}

type repositoryCloneFunc func(context.Context, Repository, CloneRequest, Credential) (CloneResult, error)

type submoduleCloneState struct {
	client          *Client
	ctx             context.Context
	depth           int
	onProgress      func(ProgressEvent)
	onEvent         func(OperationEvent)
	cloneRepository repositoryCloneFunc
}

func (client *Client) cloneSubmodules(ctx context.Context, request CloneRequest, repository Repository, credential Credential, destination string) error {
	state := submoduleCloneState{
		client:          client,
		ctx:             ctx,
		depth:           request.Depth,
		onProgress:      request.OnProgress,
		onEvent:         request.OnEvent,
		cloneRepository: client.cloneWithFallback,
	}
	return state.clone(repository, credential, destination, 0)
}

func (state *submoduleCloneState) clone(repository Repository, credential Credential, parentPath string, nesting int) error {
	checkouts, err := readSubmoduleCheckouts(parentPath, repository)
	if err != nil {
		return fmt.Errorf("inspect Git submodules: %w", err)
	}
	if len(checkouts) == 0 {
		return nil
	}
	if nesting >= maxSubmoduleDepth {
		return ErrSubmoduleLimit
	}
	for _, checkout := range checkouts {
		if err := state.ctx.Err(); err != nil {
			return err
		}
		childPath, err := submoduleDestination(parentPath, checkout.Path)
		if err != nil {
			return fmt.Errorf("prepare Git submodule path: %w", err)
		}
		childRepository, childCredential, err := state.client.prepare(state.ctx, checkout.URL)
		if err != nil {
			return fmt.Errorf("validate Git submodule repository: %w", err)
		}
		request := CloneRequest{
			URL:         childRepository.URL,
			Commit:      checkout.Commit,
			Destination: childPath,
			Depth:       state.depth,
			OnProgress:  state.onProgress,
			OnEvent:     state.onEvent,
		}
		result, err := state.cloneRepository(state.ctx, childRepository, request, childCredential)
		if err != nil {
			return fmt.Errorf("clone Git submodule %q: %w", checkout.Path, err)
		}
		if !strings.EqualFold(result.Commit, checkout.Commit) {
			return fmt.Errorf("Git submodule %q: %w", checkout.Path, ErrInvalidReference)
		}
		if err := state.clone(childRepository, childCredential, childPath, nesting+1); err != nil {
			return fmt.Errorf("clone nested Git submodule %q: %w", checkout.Path, err)
		}
	}
	return nil
}

func readSubmoduleCheckouts(repositoryPath string, repository Repository) ([]submoduleCheckout, error) {
	cloned, err := git.PlainOpen(repositoryPath)
	if err != nil {
		return nil, err
	}
	head, err := cloned.Head()
	if err != nil {
		return nil, err
	}
	commit, err := cloned.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	gitlinks := make(map[string]string)
	if err := collectGitlinks(tree, "", gitlinks); err != nil {
		return nil, err
	}
	if len(gitlinks) == 0 {
		return nil, nil
	}
	modulesFile, err := tree.File(".gitmodules")
	if err != nil {
		return nil, errors.New("Git submodule entries have no .gitmodules file")
	}
	modulesContents, err := modulesFile.Contents()
	if err != nil {
		return nil, err
	}
	modules := gitconfig.NewModules()
	if err := modules.Unmarshal([]byte(modulesContents)); err != nil {
		return nil, errors.New("invalid .gitmodules configuration")
	}
	modulesByPath := make(map[string]*gitconfig.Submodule, len(modules.Submodules))
	for _, module := range modules.Submodules {
		if module == nil {
			continue
		}
		if err := validateSubmodulePath(module.Path); err != nil {
			return nil, err
		}
		if _, exists := modulesByPath[module.Path]; exists {
			return nil, errors.New("duplicate Git submodule path")
		}
		modulesByPath[module.Path] = module
	}

	paths := make([]string, 0, len(gitlinks))
	for modulePath := range gitlinks {
		paths = append(paths, modulePath)
	}
	sort.Strings(paths)
	seenPortablePaths := make(map[string]struct{}, len(paths))
	checkouts := make([]submoduleCheckout, 0, len(paths))
	for _, modulePath := range paths {
		if err := validateSubmodulePath(modulePath); err != nil {
			return nil, err
		}
		portablePath := strings.ToLower(modulePath)
		if _, exists := seenPortablePaths[portablePath]; exists {
			return nil, errors.New("Git submodule paths collide on a case-insensitive filesystem")
		}
		seenPortablePaths[portablePath] = struct{}{}
		module := modulesByPath[modulePath]
		if module == nil {
			return nil, errors.New("Git submodule entry is missing from .gitmodules")
		}
		if err := module.Validate(); err != nil {
			return nil, errors.New("invalid Git submodule entry")
		}
		moduleURL, err := resolveSubmoduleURL(repository.URL, module.URL)
		if err != nil {
			return nil, err
		}
		checkouts = append(checkouts, submoduleCheckout{Path: modulePath, URL: moduleURL, Commit: gitlinks[modulePath]})
	}
	return checkouts, nil
}

func collectGitlinks(tree *object.Tree, parentPath string, gitlinks map[string]string) error {
	for _, entry := range tree.Entries {
		entryPath := entry.Name
		if parentPath != "" {
			entryPath = path.Join(parentPath, entry.Name)
		}
		switch entry.Mode {
		case filemode.Submodule:
			if _, exists := gitlinks[entryPath]; exists {
				return errors.New("duplicate Git submodule tree entry")
			}
			gitlinks[entryPath] = entry.Hash.String()
		case filemode.Dir:
			nestedTree, err := tree.Tree(entry.Name)
			if err != nil {
				return err
			}
			if err := collectGitlinks(nestedTree, entryPath, gitlinks); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveSubmoduleURL(parentURL, moduleURL string) (string, error) {
	moduleURL = strings.TrimSpace(moduleURL)
	if moduleURL == "" {
		return "", ErrInvalidURL
	}
	if absolute, err := ParseURL(moduleURL); err == nil {
		return absolute.URL, nil
	}
	if strings.Contains(moduleURL, "://") || strings.ContainsAny(moduleURL, "\\?#\x00") || strings.HasPrefix(moduleURL, "/") {
		return "", ErrInvalidURL
	}
	if strings.ContainsAny(moduleURL, " \t\r\n:") {
		return "", ErrInvalidURL
	}
	parent, err := ParseURL(parentURL)
	if err != nil {
		return "", err
	}
	if strings.Contains(parent.URL, "://") {
		base, err := url.Parse(parent.URL)
		if err != nil {
			return "", ErrInvalidURL
		}
		base.Path = strings.TrimSuffix(base.Path, "/") + "/"
		reference, err := url.Parse(moduleURL)
		if err != nil || reference.IsAbs() || reference.Host != "" || reference.RawQuery != "" || reference.Fragment != "" {
			return "", ErrInvalidURL
		}
		resolved, err := ParseURL(base.ResolveReference(reference).String())
		if err != nil || resolved.Host != parent.Host {
			return "", ErrInvalidURL
		}
		return resolved.URL, nil
	}
	left, parentPath, found := strings.Cut(parent.URL, ":")
	if !found {
		return "", ErrInvalidURL
	}
	resolvedPath := path.Clean(strings.Trim(parentPath, "/") + "/" + moduleURL)
	if resolvedPath == "." || resolvedPath == ".." || strings.HasPrefix(resolvedPath, "../") || path.IsAbs(resolvedPath) {
		return "", ErrInvalidURL
	}
	resolved, err := ParseURL(left + ":" + resolvedPath)
	if err != nil || resolved.Host != parent.Host {
		return "", ErrInvalidURL
	}
	return resolved.URL, nil
}

func validateSubmodulePath(modulePath string) error {
	if modulePath == "" || strings.ContainsRune(modulePath, 0) || strings.Contains(modulePath, "\\") || strings.ContainsAny(modulePath, ":") {
		return errors.New("unsafe Git submodule path")
	}
	if path.IsAbs(modulePath) || filepath.IsAbs(modulePath) || path.Clean(modulePath) != modulePath {
		return errors.New("unsafe Git submodule path")
	}
	for _, segment := range strings.Split(modulePath, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.EqualFold(segment, ".git") || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") || strings.ContainsAny(segment, "<>\"|?*") || isWindowsReservedPath(segment) {
			return errors.New("unsafe Git submodule path")
		}
	}
	return nil
}

func isWindowsReservedPath(segment string) bool {
	base := strings.ToUpper(segment)
	if extension := strings.IndexByte(base, '.'); extension >= 0 {
		base = base[:extension]
	}
	base = strings.TrimRight(base, " .")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9'
}

func submoduleDestination(parentPath, modulePath string) (string, error) {
	if err := validateSubmodulePath(modulePath); err != nil {
		return "", err
	}
	root, err := filepath.Abs(parentPath)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", errors.New("invalid Git submodule parent directory")
	}
	destination := filepath.Join(root, filepath.FromSlash(modulePath))
	if err := withinRoot(root, destination); err != nil {
		return "", errors.New("Git submodule path escapes its parent repository")
	}
	relative, err := filepath.Rel(root, destination)
	if err != nil {
		return "", err
	}
	segments := strings.Split(relative, string(filepath.Separator))
	current := root
	for index, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if index < len(segments)-1 {
				if err := os.Mkdir(current, 0o700); err != nil {
					return "", err
				}
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("Git submodule path traverses a non-directory")
		}
		if index == len(segments)-1 {
			entries, err := os.ReadDir(current)
			if err != nil {
				return "", err
			}
			if len(entries) != 0 {
				return "", errors.New("Git submodule destination is not empty")
			}
			if err := os.Remove(current); err != nil {
				return "", err
			}
		}
	}
	return destination, nil
}
