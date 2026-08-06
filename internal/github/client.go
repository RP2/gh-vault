package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	gh "github.com/google/go-github/v69/github"
	"golang.org/x/oauth2"
)

type Client interface {
	ListAccessibleRepos(ctx context.Context) ([]*gh.Repository, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repository, error)
	RateLimitStatus(ctx context.Context) (*gh.Rate, error)
}

type GhClient struct {
	tokenProvider TokenProvider
}

func NewClient(tp TokenProvider) *GhClient {
	return &GhClient{tokenProvider: tp}
}

var _ Client = (*GhClient)(nil)

func (c *GhClient) newGitHubClient(ctx context.Context) (*gh.Client, error) {
	token, err := c.tokenProvider.GetToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: get token: %w", err)
	}
	if token == "" {
		return nil, errors.New("github: token not configured")
	}
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, src)
	return gh.NewClient(httpClient), nil
}

func (c *GhClient) ListAccessibleRepos(ctx context.Context) ([]*gh.Repository, error) {
	client, err := c.newGitHubClient(ctx)
	if err != nil {
		return nil, err
	}

	slog.Info("github: listing repos", "visibility", "all")

	// Classic PATs and fine-grained tokens both work with the unfiltered
	// endpoint, but fine-grained tokens may default to public repos unless
	// visibility is explicitly set to "all".
	opts := &gh.RepositoryListByAuthenticatedUserOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
		Visibility:  "all",
	}

	accessible, err := c.listReposWithOpts(ctx, client, opts)
	if err != nil {
		return nil, err
	}

	// Fallback: some fine-grained tokens may need affiliation=owner to
	// return any repositories. Only retry if the first attempt was empty.
	if len(accessible) == 0 {
		slog.Debug("github: first list returned 0 repos, retrying with affiliation=owner")
		opts = &gh.RepositoryListByAuthenticatedUserOptions{
			ListOptions: gh.ListOptions{PerPage: 100},
			Visibility:  "all",
			Affiliation: "owner",
		}
		accessible, err = c.listReposWithOpts(ctx, client, opts)
		if err != nil {
			return nil, err
		}
	}

	slog.Info("github: listed repos", "total", len(accessible))
	return accessible, nil
}

func (c *GhClient) listReposWithOpts(ctx context.Context, client *gh.Client, opts *gh.RepositoryListByAuthenticatedUserOptions) ([]*gh.Repository, error) {
	var accessible []*gh.Repository
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("github: list repos cancelled: %w", ctx.Err())
		}
		repos, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list accessible repos: %w", err)
		}
		slog.Debug("github: page", "repos_on_page", len(repos), "page", opts.Page)
		for _, r := range repos {
			slog.Debug("github: repo",
				"id", r.GetID(),
				"name", r.GetOwner().GetLogin()+"/"+r.GetName(),
				"private", r.GetPrivate(),
				"archived", r.GetArchived(),
			)
			if r.GetFork() {
				continue
			}
			accessible = append(accessible, r)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return accessible, nil
}

func (c *GhClient) GetRepo(ctx context.Context, owner, name string) (*gh.Repository, error) {
	client, err := c.newGitHubClient(ctx)
	if err != nil {
		return nil, err
	}
	repo, _, err := client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return nil, fmt.Errorf("github: get repo %s/%s: %w", owner, name, err)
	}
	return repo, nil
}

func (c *GhClient) RateLimitStatus(ctx context.Context) (*gh.Rate, error) {
	client, err := c.newGitHubClient(ctx)
	if err != nil {
		return nil, err
	}
	limits, _, err := client.RateLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: rate limits: %w", err)
	}
	if limits == nil || limits.Core == nil {
		return nil, errors.New("github: rate limits response missing core bucket")
	}
	return limits.Core, nil
}
