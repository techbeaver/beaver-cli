package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP resources, as distinct from tools.
//
// The read-heavy, cacheable things a client should be able to subscribe to
// rather than poll: the plan catalogues, the region list, the customer's own
// projects. Clients that support resource subscriptions get told when these
// change instead of holding a stale list for a whole session, which matters
// because every one of them is operator-editable data.
//
// They duplicate what list_plans and list_regions return on purpose. A tool is
// what a model calls when it decides to; a resource is what a client can keep
// current on its own.

const (
	uriPaaSPlans  = "techbeaver://catalogue/app-plans"
	uriDBaaSPlans = "techbeaver://catalogue/database-plans"
	uriRegions    = "techbeaver://catalogue/regions"
	uriProjects   = "techbeaver://account/projects"
)

func addResources(server *mcpsdk.Server, deps *Deps, identity *Identity) {
	catalogue := []struct {
		uri, name, title, description, path string
		scopes                              []string
	}{
		{
			uri: uriPaaSPlans, name: "app-plans", title: "Application plans",
			description: "The application plans available right now, with their current prices and limits. Read this rather than recalling a price: plans are operator-editable and a plan can be retired or repriced at any time.",
			path:        "/paas/plans", scopes: []string{scopes.ProjectsRead},
		},
		{
			uri: uriDBaaSPlans, name: "database-plans", title: "Managed database plans",
			description: "The managed PostgreSQL plans available right now. A plan that is not listed here cannot be bought, whatever else you may have been told.",
			path:        "/dbaas/plans", scopes: []string{scopes.ProjectsRead},
		},
		{
			uri: uriRegions, name: "regions", title: "Regions",
			description: "The regions that can actually host a project today. This is reconciled against which clusters are live, so it is shorter than a marketing page and it is the one that is true.",
			path:        "/paas/regions", scopes: []string{scopes.ProjectsRead},
		},
		{
			uri: uriProjects, name: "projects", title: "Your projects",
			description: "The projects on this account. Apps and databases both live inside a project, so this is usually the first thing to read.",
			path:        "/paas/projects", scopes: []string{scopes.ProjectsRead},
		},
	}

	for _, entry := range catalogue {
		if !identity.HasAll(entry.scopes) {
			continue
		}
		path := entry.path
		uri := entry.uri
		server.AddResource(&mcpsdk.Resource{
			URI:         uri,
			Name:        entry.name,
			Title:       entry.title,
			Description: entry.description,
			MIMEType:    "application/json",
		}, func(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			session, err := SessionFrom(ctx)
			if err != nil {
				return nil, err
			}
			env, err := deps.Client.Do(ctx, session.Token, client.Request{Method: http.MethodGet, Path: path})
			if err != nil {
				return nil, describeAPIError(err)
			}
			body := env.Data
			if len(body) == 0 {
				body = json.RawMessage("[]")
			}
			return &mcpsdk.ReadResourceResult{
				Contents: []*mcpsdk.ResourceContents{{
					URI:      uri,
					MIMEType: "application/json",
					Text:     string(body),
				}},
			}, nil
		})
	}
}
