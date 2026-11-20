package gitops

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

var commitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func parseReferences(repository Repository, requested string, output []byte, engine string) (ProbeResult, error) {
	result := ProbeResult{Repository: repository, CheckedAt: time.Now().UTC(), Engine: engine}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			result.DefaultBranch = strings.TrimPrefix(fields[1], "refs/heads/")
			continue
		}
		if len(fields) != 2 || !commitPattern.MatchString(fields[0]) {
			continue
		}
		if strings.Trim(fields[0], "0") == "" {
			continue
		}
		refs[fields[1]] = fields[0]
	}
	for name := range refs {
		if strings.HasSuffix(name, "^{}") {
			refs[strings.TrimSuffix(name, "^{}")] = refs[name]
		}
	}
	for name, commit := range refs {
		if strings.HasSuffix(name, "^{}") {
			continue
		}
		kind := "other"
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			kind = "branch"
		case strings.HasPrefix(name, "refs/tags/"):
			kind = "tag"
		}
		result.References = append(result.References, Reference{Name: strings.TrimPrefix(strings.TrimPrefix(name, "refs/heads/"), "refs/tags/"), Type: kind, Commit: commit})
		if kind == "branch" {
			result.BranchCount++
		}
		if kind == "tag" {
			result.TagCount++
		}
	}
	sortReferences(result.References)
	if len(refs) == 0 {
		return result, ErrNoReferences
	}
	resolved := requested
	if resolved == "" {
		resolved = result.DefaultBranch
	}
	if resolved == "" {
		if refs["HEAD"] == "" {
			return result, fmt.Errorf("%w: remote did not advertise its default branch", ErrUnsupported)
		}
		resolved = "HEAD"
	}
	result.ResolvedRef = resolved
	result.ResolvedCommit = refs[resolved]
	if result.ResolvedCommit == "" {
		result.ResolvedCommit = refs["refs/heads/"+resolved]
	}
	if result.ResolvedCommit == "" {
		result.ResolvedCommit = refs["refs/tags/"+resolved]
	}
	if result.ResolvedCommit == "" && isCommit(requested) {
		result.ResolvedCommit = requested
		result.CommitValidationDeferred = true
		for _, commit := range refs {
			if strings.EqualFold(commit, requested) {
				result.CommitValidationDeferred = false
				break
			}
		}
	}
	if result.ResolvedCommit == "" {
		return result, fmt.Errorf("%w: %s", ErrInvalidReference, requested)
	}
	return result, nil
}

func limitReferences(result *ProbeResult, maximum int) {
	if maximum <= 0 || len(result.References) <= maximum {
		return
	}
	result.References = result.References[:maximum]
}

func isCommit(value string) bool { return commitPattern.MatchString(value) }

func validateRequestedRef(value string) error {
	if value == "" || isCommit(value) {
		return nil
	}
	if strings.HasPrefix(value, "-") || plumbing.NewBranchReferenceName(value).Validate() != nil || plumbing.NewTagReferenceName(value).Validate() != nil {
		return ErrInvalidReference
	}
	return nil
}
