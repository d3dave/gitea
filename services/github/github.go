package github

import (
	"context"
	"errors"
	"os"
	"regexp"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"

	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/git"
)

var client *githubv4.Client
var ownerNameRegex *regexp.Regexp = regexp.MustCompile(`github.com/([\w-_.]+)/([\w-_.]+)\.git$`)

func init() {
	token := oauth2.Token{AccessToken: os.Getenv("GH_TOKEN")}
	tokenSource := oauth2.StaticTokenSource(&token)
	httpClient := oauth2.NewClient(context.Background(), tokenSource)
	client = githubv4.NewClient(httpClient)
}

func GetMirrorOwnerAndName(ctx context.Context, repo *repo_model.Repository) (string, string, error) {
	mirror, err := repo_model.GetMirrorByRepoID(ctx, repo.ID)
	if err != nil {
		return "", "", err
	}

	address, err := git.GetRemoteAddress(ctx, repo.RepoPath(), mirror.GetRemoteName())
	if err != nil {
		return "", "", err
	}

	parts := ownerNameRegex.FindStringSubmatch(address)
	if parts == nil {
		return "", "", errors.New("not a github mirror")
	}

	return parts[1], parts[2], nil
}

func Query(ctx context.Context, q any, variables map[string]any) error {
	return client.Query(ctx, q, variables)
}
