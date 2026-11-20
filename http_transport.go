package gitops

import (
	"context"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const gitOpsHTTPSProtocol = "gitops+https"

type gitHTTPRedirectPolicyKey struct{}

type gitHTTPRedirectPolicy struct {
	allowedHosts []string
	validateHost func(context.Context, string) error
}

type gitOpsHTTPSClient struct {
	delegate transport.Transport
}

func init() {
	delegate := gitHTTP.NewClientWithOptions(&http.Client{
		Transport:     http.DefaultTransport,
		CheckRedirect: validateGitHTTPRedirect,
	}, &gitHTTP.ClientOptions{RedirectPolicy: gitHTTP.FollowInitialRedirects})
	transportclient.InstallProtocol(gitOpsHTTPSProtocol, gitOpsHTTPSClient{delegate: delegate})
}

func (client gitOpsHTTPSClient) NewUploadPackSession(endpoint *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	return client.delegate.NewUploadPackSession(httpsEndpoint(endpoint), auth)
}

func (client gitOpsHTTPSClient) NewReceivePackSession(endpoint *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return client.delegate.NewReceivePackSession(httpsEndpoint(endpoint), auth)
}

func httpsEndpoint(endpoint *transport.Endpoint) *transport.Endpoint {
	if endpoint == nil {
		return nil
	}
	copy := *endpoint
	copy.Protocol = "https"
	return &copy
}

func withGitHTTPRedirectPolicy(ctx context.Context, config Config) context.Context {
	return context.WithValue(ctx, gitHTTPRedirectPolicyKey{}, gitHTTPRedirectPolicy{
		allowedHosts: append([]string(nil), config.AllowedHosts...),
		validateHost: config.ValidateHost,
	})
}

func validateGitHTTPRedirect(request *http.Request, _ []*http.Request) error {
	if request == nil || request.URL == nil || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Hostname() == "" {
		return ErrHostNotAllowed
	}
	policy, ok := request.Context().Value(gitHTTPRedirectPolicyKey{}).(gitHTTPRedirectPolicy)
	if !ok {
		return ErrHostNotAllowed
	}
	host := normalizedURLHost(request.URL)
	if err := ValidateHost(host, policy.allowedHosts); err != nil {
		return err
	}
	if policy.validateHost != nil {
		return policy.validateHost(request.Context(), host)
	}
	return nil
}
