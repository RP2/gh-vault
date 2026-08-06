package github

import (
	"context"
	"errors"
	"fmt"

	gh "github.com/google/go-github/v69/github"
	"golang.org/x/oauth2"
)

type Client interface {
	ListOwnedRepos(ctx context.Context) ([]*gh.Repository, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repository, error)
	ArchiveRepo(ctx context.Context, owner, name string) error
	DeleteRepo(ctx context.Context, owner, name string) error
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

func (c *GhClient) ListOwnedRepos(ctx context.Context) ([]*gh.Repository, error) {
	client, err := c.newGitHubClient(ctx)
	if err != nil {
		return nil, err
	}

	opts := &gh.RepositoryListByAuthenticatedUserOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
		Type:        "",
	}

	var owned []*gh.Repository
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("github: list repos cancelled: %w", ctx.Err())
		}
		repos, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list owned repos: %w", err)
		}
		for _, r := range repos {
			if r.GetFork() {
				continue
			}
			owned = append(owned, r)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return owned, nil
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

func (c *GhClient) ArchiveRepo(ctx context.Context, owner, name string) error {
	client, err := c.newGitHubClient(ctx)
	if err != nil {
		return err
	}
	archived := true
	_, _, err = client.Repositories.Edit(ctx, owner, name, &gh.Repository{Archived: &archived})
	if err != nil {
		return fmt.Errorf("github: archive repo %s/%s: %w", owner, name, err)
	}
	return nil
}

func (c *GhClient) DeleteRepo(ctx context.Context, owner, name string) error {
	client, err := c.newGitHubClient(ctx)
	if err != nil {
		return err
	}
	if _, err := client.Repositories.Delete(ctx, owner, name); err != nil {
		return fmt.Errorf("github: delete repo %s/%s: %w", owner, name, err)
	}
	return nil
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
