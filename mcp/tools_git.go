package mcp

import (
	"context"
	"fmt"
	"net/url"

	"github.com/techbeaver/beaver-cli/scopes"
)

// Whether this platform can read a customer's repository.
//
// These tools exist because of a failure that was invisible from the agent's
// side and expensive from the customer's. Deploying a private repository
// depends on the TechBeaver GitHub App being installed on the account that owns
// it, and the console has always had a Connect GitHub step to arrange that. The
// tool surface had nothing: no way even to ask. An agent would call create_app
// with a private repository URL, the customer would be invoiced, the build pod
// would fail to clone, and the explanation would be sitting in a build log
// nobody had thought to read yet.
//
// So the question is asked first, and the answer carries the thing to do about
// it. The install URL an agent hands over is the App's plain installation page,
// which is stateless by design: no CSRF cookie has to travel with it, so it
// works in a chat message opened in the customer's own browser, hours later.
//
// Nothing here is a write. Installing the App is the customer's act on GitHub's
// domain, and it could not be otherwise: only an owner of the account can grant
// access to that account's code.

type repoAccessInput struct {
	GitRepo string `json:"gitRepo" jsonschema:"the https URL of the git repository, e.g. https://github.com/owner/name"`
}

func registerGitTools(r *Registry) {
	register(r, toolSpec{
		Name:     "check_repo_access",
		Title:    "Check whether a repository can be deployed",
		Scopes:   []string{scopes.PaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "Whether TechBeaver can clone a repository, and what the customer must do if it cannot. Call this BEFORE create_app for any repository you have not deployed before: creating an app on a priced plan raises an invoice, and a private repository the platform cannot read will fail to build after the customer has already paid. A public repository needs nothing. A private one needs the TechBeaver GitHub App installed on the account that owns it, which only an owner of that account can do.",
	}, func(ctx context.Context, deps *Deps, call *Call, in repoAccessInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/paas/git/access", url.Values{"repo": {in.GitRepo}})
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: repoAccessSummary(item),
			Item:    item,
			// The install link, not the detail sentence: a link inside a paragraph gets paraphrased away.
			NextStep: repoAccessNextStep(item),
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_git_connections",
		Title:    "List connected GitHub accounts",
		Scopes:   []string{scopes.PaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The GitHub accounts and organisations this customer has installed the TechBeaver GitHub App on. Repositories owned by an account that is not listed here cannot be cloned unless they are public. If this fails because the customer has not connected GitHub at all, say so and give them the install link from check_repo_access.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/paas/git/github/installations", nil)
		if err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("%d connected GitHub account(s).", len(items))
		if len(items) == 0 {
			summary = "No GitHub account is connected. Only public repositories can be deployed until one is."
		}
		return &ListResult{Summary: summary, Count: len(items), Items: items}, nil
	})
}

// repoAccessSummary states the answer in one line, in the terms the customer
// will hear it in.
func repoAccessSummary(item map[string]any) string {
	name := pick(item, "owner")
	if repo := pick(item, "repo"); repo != "" && name != "" {
		name += "/" + repo
	}
	if name == "" {
		name = "that repository"
	}

	switch {
	case pick(item, "reachable") == "true" && pick(item, "public") == "true":
		return fmt.Sprintf("%s is public, so it can be deployed with no connection at all.", name)
	case pick(item, "reachable") == "true":
		return fmt.Sprintf("%s can be deployed: TechBeaver has access to it.", name)
	case pick(item, "checked") != "true":
		return fmt.Sprintf("Whether %s can be deployed cannot be checked in advance. %s", name, pick(item, "detail"))
	default:
		return fmt.Sprintf("%s cannot be deployed yet. %s", name, pick(item, "detail"))
	}
}

// repoAccessNextStep is empty when there is nothing to do, which is what stops
// an agent from reciting a connection procedure at somebody whose repository
// already works.
func repoAccessNextStep(item map[string]any) string {
	if pick(item, "reachable") == "true" {
		return ""
	}
	installURL := pick(item, "installUrl")

	switch pick(item, "reason") {
	case "not_installed", "repo_not_selected", "installation_suspended":
		if installURL == "" {
			return "Do not create the app yet. The customer must give TechBeaver access to this repository on GitHub first."
		}
		return fmt.Sprintf(
			"Do not create the app yet: it would raise an invoice for something that cannot build. Give the customer this link and ask them to open it themselves, as an owner of that GitHub account: %s. Then call check_repo_access again.",
			installURL)
	case "app_not_configured":
		return "This is a platform problem, not the customer's. Tell them to contact TechBeaver support rather than trying to fix it on GitHub."
	case "not_a_git_url":
		return "Ask the customer for the repository's https URL, in the form https://github.com/owner/name."
	default:
		return "Do not create the app until this is resolved, or the customer may pay for an app that cannot build."
	}
}

// repoMustBeReachable stops an app being created against code this platform
// cannot read.
//
// The order of events is the whole argument. create_app on a priced plan raises
// an invoice immediately, and the clone happens later in a build pod, so
// without this the sequence was: agent creates the app, customer pays, build
// fails on a repository nobody could ever have cloned, and the explanation is
// in a log. The customer is then out the money for an app that has never run,
// and the fix is on GitHub, where only they can perform it.
//
// It refuses only on a definite no. A repository whose reachability could not
// be determined, and any failure of the check itself, let the creation proceed:
// a pre-flight that blocks real work when it is the one that is broken is worse
// than the problem it was added for. That is why the check is skipped entirely
// for an app with no repository, which is how the AI builder deploys, and why a
// failed check is swallowed: it covers the customer who granted paas:write but
// not paas:read, so the check is not even permitted, which is no reason to
// refuse to do the thing they did grant.
//
// The refusal set is the four answers that mean the customer has to do
// something on GitHub. "github_error" is GitHub being unreachable or refusing
// to mint a token, and "app_not_configured" is our own deployment being wrong.
// Neither is evidence about this repository, and turning either into a refusal
// would mean a GitHub outage stops customers creating apps at all, including
// from public repositories that would clone perfectly well.
func repoMustBeReachable(ctx context.Context, deps *Deps, call *Call, gitRepo string) error {
	if gitRepo == "" {
		return nil
	}

	item, err := apiGet(ctx, deps, call, "/paas/git/access", url.Values{"repo": {gitRepo}})
	if err != nil {
		return nil
	}
	if pick(item, "checked") != "true" || pick(item, "reachable") == "true" {
		return nil
	}

	definite := map[string]bool{
		"not_installed":          true,
		"repo_not_selected":      true,
		"installation_suspended": true,
		"not_a_git_url":          true,
	}
	if !definite[pick(item, "reason")] {
		return nil
	}

	detail := pick(item, "detail")
	if detail == "" {
		detail = "TechBeaver cannot read that repository."
	}
	return fmt.Errorf("%s\n\nNothing has been created and nothing has been charged. %s",
		detail, repoAccessNextStep(item))
}
