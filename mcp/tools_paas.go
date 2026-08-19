package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

// Applications: creating them, configuring them, deploying them, and reading
// what they are doing.
//
// Deleting one lives in tools_destructive.go, with the human gate.

type appIdInput struct {
	AppId string `json:"appId" jsonschema:"the app's id, from list_apps"`
}

type createAppInput struct {
	ProjectId string `json:"projectId" jsonschema:"the project to create it in, from list_projects"`
	Name      string `json:"name" jsonschema:"a name for the app, 3 to 50 characters"`
	GitRepo   string `json:"gitRepo,omitempty" jsonschema:"the https URL of the git repository to deploy from"`
	Branch    string `json:"branch,omitempty" jsonschema:"which branch to deploy; defaults to the repository's default"`
	Subdomain string `json:"subdomain,omitempty" jsonschema:"the subdomain to publish on; one is derived from the name if omitted"`
	Framework string `json:"framework,omitempty" jsonschema:"nixpacks to build automatically, or dockerfile to use a Dockerfile in the repository"`
	Port      int    `json:"port,omitempty" jsonschema:"the port the app listens on inside its container"`
	PlanId    string `json:"planId,omitempty" jsonschema:"which plan to buy, from list_plans; omitted means the cheapest active plan"`
	// BillingCycle is not defaulted here on purpose: a plan may have no price
	// for a cycle, and the API refuses that rather than treating it as free.
	BillingCycle   string            `json:"billingCycle,omitempty" jsonschema:"monthly or yearly"`
	EnvVars        map[string]string `json:"envVars,omitempty" jsonschema:"environment variables the running app will see"`
	BuildEnv       map[string]string `json:"buildEnv,omitempty" jsonschema:"environment variables available only while building"`
	IdempotencyKey string            `json:"idempotencyKey" jsonschema:"a unique string you generate once for this creation; reuse exactly the same value if you retry, or the customer may be charged twice"`
}

type setEnvInput struct {
	AppId string            `json:"appId" jsonschema:"the app's id"`
	Vars  map[string]string `json:"vars" jsonschema:"the variables to set; existing ones not named here are left alone"`
}

type getEnvInput struct {
	AppId string `json:"appId" jsonschema:"the app's id"`
	// IncludeValues exists so that reading a secret is an explicit act rather
	// than a side effect of asking what is configured.
	IncludeValues bool `json:"includeValues,omitempty" jsonschema:"set true ONLY if the customer asked for the actual values; the values are secrets and will appear in this conversation"`
}

func registerAppTools(r *Registry) {
	register(r, toolSpec{
		Name:     "get_app",
		Title:    "Get an app",
		Scopes:   []string{scopes.PaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "One application's status, plan, URL and configuration.",
	}, func(ctx context.Context, deps *Deps, call *Call, in appIdInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/paas/apps/"+url.PathEscape(in.AppId), nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: fmt.Sprintf("App %s is %s.", pick(item, "name", "id"), pick(item, "status")),
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:        "create_app",
		Title:       "Create an app",
		Scopes:      []string{scopes.PaaSWrite},
		Description: "Creates an application in a project and queues its first build. On a priced plan this raises an invoice and the app stays in pending_payment until the customer pays: use create_checkout_link and hand them the payment link. Call list_plans first rather than assuming a plan id.",
	}, func(ctx context.Context, deps *Deps, call *Call, in createAppInput) (*ActionResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		body := map[string]any{"projectId": in.ProjectId, "name": in.Name}
		putIfSet(body, "gitRepo", in.GitRepo)
		putIfSet(body, "branch", in.Branch)
		putIfSet(body, "subdomain", in.Subdomain)
		putIfSet(body, "framework", in.Framework)
		putIfSet(body, "paasPlanId", in.PlanId)
		putIfSet(body, "billingCycle", in.BillingCycle)
		if in.Port > 0 {
			body["port"] = in.Port
		}
		if len(in.EnvVars) > 0 {
			body["envVars"] = in.EnvVars
		}
		if len(in.BuildEnv) > 0 {
			body["buildEnv"] = in.BuildEnv
		}

		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/apps", body, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		result := &ActionResult{
			Summary: fmt.Sprintf("Created app %q.", in.Name),
			Result:  item,
		}
		if pick(item, "status") == "pending_payment" {
			result.NextStep = "This app is on a priced plan and will not deploy until it is paid for. Call create_checkout_link for it and give the customer the payment link; you cannot pay on their behalf."
		}
		return result, nil
	})

	register(r, toolSpec{
		Name:        "deploy_app",
		Title:       "Deploy an app",
		Scopes:      []string{scopes.PaaSDeploy},
		Description: "Builds and deploys the app's current branch. Deployment is asynchronous: poll get_app for status and read get_build_logs if it fails, rather than deploying repeatedly.",
	}, func(ctx context.Context, deps *Deps, call *Call, in appIdInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/apps/"+url.PathEscape(in.AppId)+"/deploy", map[string]any{}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  "Deployment queued.",
			Result:   item,
			NextStep: "Poll get_app for the status, and read get_build_logs if it does not come up.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "rollback_app",
		Title:       "Roll a deployment back",
		Scopes:      []string{scopes.PaaSDeploy},
		Description: "Puts the previous working deployment back. Use this when a deploy broke something in production; it is the fast way back and it destroys nothing.",
	}, func(ctx context.Context, deps *Deps, call *Call, in appIdInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/apps/"+url.PathEscape(in.AppId)+"/rollback", map[string]any{}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: "Rollback started.", Result: item}, nil
	})

	register(r, toolSpec{
		Name:        "stop_app",
		Title:       "Stop an app",
		Scopes:      []string{scopes.PaaSWrite},
		Destructive: true,
		Description: "Takes the running app down. Reversible: the app record, its configuration and its data are untouched, and start_app brings it back. This is the right tool when a customer wants something to stop costing them attention; delete_app is not.",
	}, func(ctx context.Context, deps *Deps, call *Call, in appIdInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/apps/"+url.PathEscape(in.AppId)+"/stop", map[string]any{}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  "App stopped. Nothing was deleted.",
			Result:   item,
			NextStep: "Call start_app to bring it back up.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "start_app",
		Title:       "Start an app",
		Scopes:      []string{scopes.PaaSWrite},
		Description: "Brings a stopped app back up on the plan it already has. Reverses stop_app. It does not redeploy: the app comes back on the build it was running when it was stopped.",
	}, func(ctx context.Context, deps *Deps, call *Call, in appIdInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/apps/"+url.PathEscape(in.AppId)+"/start", map[string]any{}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: "App starting.", Result: item}, nil
	})

	register(r, toolSpec{
		Name:     "get_env_vars",
		Title:    "Read environment variables",
		Scopes:   []string{scopes.PaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "Which environment variables an app has. Values are hidden unless you explicitly ask for them, because they are secrets and this conversation may be read by someone else. Ask for values only when the customer asked for a value.",
	}, func(ctx context.Context, deps *Deps, call *Call, in getEnvInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/paas/apps/"+url.PathEscape(in.AppId)+"/env", nil)
		if err != nil {
			return nil, err
		}
		if !in.IncludeValues {
			item = redactValues(item)
			return &ObjectResult{
				Summary: fmt.Sprintf("%d environment variable(s). Values are hidden; ask again with includeValues if the customer wants one.", len(item)),
				Item:    item,
			}, nil
		}
		return &ObjectResult{
			Summary: fmt.Sprintf("%d environment variable(s), values included. Warn the customer that these are now in this conversation.", len(item)),
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:        "set_env_vars",
		Title:       "Set environment variables",
		Scopes:      []string{scopes.PaaSWrite},
		Description: "Sets or updates environment variables. Variables not named are left alone. The app has to be redeployed for the change to reach the running container.",
	}, func(ctx context.Context, deps *Deps, call *Call, in setEnvInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPatch, "/paas/apps/"+url.PathEscape(in.AppId)+"/env", in.Vars, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  fmt.Sprintf("Updated %d environment variable(s).", len(in.Vars)),
			Result:   redactValues(item),
			NextStep: "Call deploy_app for the running app to pick these up.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "set_app_port",
		Title:       "Set the container port",
		Scopes:      []string{scopes.PaaSWrite},
		Description: "Changes the port TechBeaver sends traffic to inside the container. The usual cause of an app that builds and deploys but answers nothing.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AppId string `json:"appId" jsonschema:"the app's id"`
		Port  int    `json:"port" jsonschema:"the port the app listens on"`
	}) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPatch, "/paas/apps/"+url.PathEscape(in.AppId)+"/port",
			map[string]any{"port": in.Port}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  fmt.Sprintf("Port set to %d.", in.Port),
			Result:   item,
			NextStep: "Redeploy for this to take effect.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "set_start_command",
		Title:       "Set the start command",
		Scopes:      []string{scopes.PaaSWrite},
		Description: "Overrides the command the container runs. Needed when a project's default start script is a development one: those depend on packages that are pruned from a production build, so the app builds and then fails to start.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AppId        string `json:"appId" jsonschema:"the app's id"`
		StartCommand string `json:"startCommand" jsonschema:"the command to run, for example: node dist/main"`
	}) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPatch, "/paas/apps/"+url.PathEscape(in.AppId)+"/start-command",
			map[string]any{"startCommand": in.StartCommand}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  "Start command updated.",
			Result:   item,
			NextStep: "Redeploy for this to take effect.",
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_custom_domains",
		Title:    "List custom domains",
		Scopes:   []string{scopes.PaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The customer's own domains attached to an app, and whether each is verified.",
	}, func(ctx context.Context, deps *Deps, call *Call, in appIdInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/paas/apps/"+url.PathEscape(in.AppId)+"/custom-domains", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{Summary: fmt.Sprintf("%d custom domain(s).", len(items)), Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:        "add_custom_domain",
		Title:       "Add a custom domain",
		Scopes:      []string{scopes.PaaSWrite},
		Description: "Attaches the customer's own domain to an app. The response carries the DNS records they must create. Read those back exactly as given rather than describing them from memory: the record types are not the ones people expect, and getting one wrong costs a round trip through DNS propagation.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AppId  string `json:"appId" jsonschema:"the app's id"`
		Domain string `json:"domain" jsonschema:"the domain name, for example shop.example.com"`
	}) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/apps/"+url.PathEscape(in.AppId)+"/custom-domains",
			map[string]any{"domain": in.Domain}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  fmt.Sprintf("Added %s. It is not live until its DNS records exist and it is verified.", in.Domain),
			Result:   item,
			NextStep: "Give the customer the DNS records in this response exactly as they appear, then call verify_custom_domain once they have created them.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "verify_custom_domain",
		Title:       "Verify a custom domain",
		Scopes:      []string{scopes.PaaSWrite},
		Idempotent:  true,
		Description: "Checks whether a custom domain's DNS records are in place and finishes setting it up. Safe to call repeatedly while DNS propagates.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AppId    string `json:"appId" jsonschema:"the app's id"`
		DomainId string `json:"domainId" jsonschema:"the domain's id, from list_custom_domains"`
	}) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/paas/apps/"+url.PathEscape(in.AppId)+"/custom-domains/"+url.PathEscape(in.DomainId)+"/verify",
			map[string]any{}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: "Verification attempted.", Result: item}, nil
	})
}

// putIfSet adds a value only when it is non-empty, so an omitted optional
// argument is genuinely omitted rather than sent as an empty string the API has
// to interpret.
func putIfSet(body map[string]any, key, value string) {
	if value != "" {
		body[key] = value
	}
}
