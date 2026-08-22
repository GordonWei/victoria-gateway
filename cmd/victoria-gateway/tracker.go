package main

import (
	"fmt"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/gitea"
	"github.com/gordonwei/victoria-gateway/pkg/github"
	"github.com/gordonwei/victoria-gateway/pkg/tracker"
)

// buildTracker constructs whichever issue tracker rag.gitea/rag.github
// configures, or returns a nil Tracker if neither is set — capture then
// just records Pending rows with no linked issue, and only `note --id`
// can confirm them. Shared by runServe and runSync so both wire the
// configured backend the same way.
func buildTracker(ragCfg *config.RAGConfig) (tracker.Tracker, error) {
	if ragCfg == nil {
		return nil, nil
	}
	if ragCfg.Gitea != nil && ragCfg.GitHub != nil {
		return nil, fmt.Errorf("rag.gitea and rag.github are both set — configure at most one issue tracker")
	}
	if ragCfg.Gitea != nil {
		return gitea.NewClient(gitea.ClientConfig{
			Endpoint: ragCfg.Gitea.Endpoint,
			Token:    ragCfg.Gitea.Token,
			Owner:    ragCfg.Gitea.Owner,
			Repo:     ragCfg.Gitea.Repo,
		}), nil
	}
	if ragCfg.GitHub != nil {
		return github.NewClient(github.ClientConfig{
			Endpoint: ragCfg.GitHub.Endpoint,
			Token:    ragCfg.GitHub.Token,
			Owner:    ragCfg.GitHub.Owner,
			Repo:     ragCfg.GitHub.Repo,
		}), nil
	}
	return nil, nil
}
