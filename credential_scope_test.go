package gitops

import (
	"context"
	"testing"
)

func TestCredentialResolutionUsesExplicitThenRepositoryThenLegacy(t *testing.T) {
	var resolved Repository
	client, err := NewClient(Config{
		CredentialForRepository: func(repository Repository) (Credential, error) {
			resolved = repository
			return Credential{Token: "repository-token"}, nil
		},
		CredentialFor: func(string) (Credential, error) {
			return Credential{Token: "legacy-token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	repository, credential, err := client.prepare(context.Background(), "HTTPS://Git.Example.COM/team/repo.git", &Credential{Token: "explicit-token"})
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != "explicit-token" {
		t.Fatalf("explicit credential = %q, want explicit-token", credential.Token)
	}
	if resolved.URL != "" {
		t.Fatal("repository resolver ran despite an explicit credential")
	}

	_, credential, err = client.prepare(context.Background(), repository.URL)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != "repository-token" {
		t.Fatalf("repository credential = %q, want repository-token", credential.Token)
	}
	if resolved.Host != "git.example.com" || resolved.Transport != "https" || resolved.Path != "team/repo.git" {
		t.Fatalf("repository resolver received %#v", resolved)
	}

	client.config.CredentialForRepository = func(Repository) (Credential, error) {
		return Credential{}, ErrCredentialNotConfigured
	}
	_, credential, err = client.prepare(context.Background(), repository.URL)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != "legacy-token" {
		t.Fatalf("legacy fallback credential = %q, want legacy-token", credential.Token)
	}

	client.config.CredentialForRepository = nil
	_, credential, err = client.prepare(context.Background(), repository.URL)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != "legacy-token" {
		t.Fatalf("legacy credential = %q, want legacy-token", credential.Token)
	}
}
