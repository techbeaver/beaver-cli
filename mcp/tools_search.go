package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/techbeaver/beaver-cli/scopes"
)

// search and fetch.
//
// Two thin aliases over the list and get tools. They exist because some
// connector surfaces, ChatGPT's in particular, have historically privileged
// servers that expose exactly these two names, and matching that convention
// costs almost nothing while materially affecting how well this server presents
// there.
//
// That claim about ChatGPT is [unverified] against OpenAI's current connector
// documentation. It is recorded here rather than asserted quietly, because if
// it turns out to be out of date these two tools are simply a convenient search
// and their justification changes, not their behaviour.
//
// They add no capability: everything reachable through them is reachable
// through the tools they wrap, under the same scopes, with the same ownership
// checks. A customer who never granted dbaas:read gets no databases from
// search, because search is running the same calls.

type searchInput struct {
	Query string `json:"query" jsonschema:"what to look for; matched against names, ids and statuses"`
}

type searchResult struct {
	Id    string `json:"id" jsonschema:"an identifier to pass to fetch"`
	Title string `json:"title" jsonschema:"a human readable name"`
	Type  string `json:"type" jsonschema:"what kind of thing this is"`
	Text  string `json:"text" jsonschema:"a one line description"`
}

type searchOutput struct {
	Summary string         `json:"summary" jsonschema:"one line about what was found"`
	Results []searchResult `json:"results" jsonschema:"the matches"`
}

type fetchInput struct {
	Id string `json:"id" jsonschema:"an id returned by search, of the form type:identifier"`
}

type fetchOutput struct {
	Id       string         `json:"id" jsonschema:"the id that was fetched"`
	Title    string         `json:"title" jsonschema:"a human readable name"`
	Text     string         `json:"text" jsonschema:"the full record, as JSON text"`
	Metadata map[string]any `json:"metadata" jsonschema:"the record's fields"`
}

func registerSearchTools(r *Registry) {
	register(r, toolSpec{
		Name:     "search",
		Title:    "Search this account",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "Finds projects, apps, managed databases and invoices on this account by name, id or status. A convenience over the list tools; it can see nothing they cannot.",
	}, func(ctx context.Context, deps *Deps, call *Call, in searchInput) (*searchOutput, error) {
		needle := strings.ToLower(strings.TrimSpace(in.Query))
		var results []searchResult

		projects, err := apiList(ctx, deps, call, "/paas/projects", nil)
		if err != nil {
			return nil, err
		}
		for _, project := range projects {
			projectID := pick(project, "id")
			if matches(needle, project) {
				results = append(results, searchResult{
					Id: "project:" + projectID, Type: "project",
					Title: pick(project, "name", "id"),
					Text:  fmt.Sprintf("Project in %s.", pick(project, "regionCode")),
				})
			}
			if projectID == "" {
				continue
			}
			if call.Session.Identity.Has(scopes.PaaSRead) {
				apps, _ := apiList(ctx, deps, call, "/paas/projects/"+url.PathEscape(projectID)+"/apps", nil)
				for _, app := range apps {
					if matches(needle, app) {
						results = append(results, searchResult{
							Id: "app:" + pick(app, "id"), Type: "app",
							Title: pick(app, "name", "id"),
							Text:  fmt.Sprintf("App in project %s, currently %s.", pick(project, "name"), pick(app, "status")),
						})
					}
				}
			}
			if call.Session.Identity.Has(scopes.DBaaSRead) {
				databases, _ := apiList(ctx, deps, call, "/dbaas/projects/"+url.PathEscape(projectID)+"/instances", nil)
				for _, db := range databases {
					if matches(needle, db) {
						results = append(results, searchResult{
							Id: "database:" + pick(db, "id"), Type: "database",
							Title: pick(db, "name", "id"),
							Text:  fmt.Sprintf("Managed database in project %s, currently %s.", pick(project, "name"), pick(db, "status")),
						})
					}
				}
			}
		}

		if call.Session.Identity.Has(scopes.BillingRead) {
			invoices, _ := apiList(ctx, deps, call, "/billing/invoices", nil)
			for _, invoice := range invoices {
				if matches(needle, invoice) {
					results = append(results, searchResult{
						Id: "invoice:" + pick(invoice, "id"), Type: "invoice",
						Title: pick(invoice, "invoiceNumber", "id"),
						Text:  fmt.Sprintf("Invoice, currently %s.", pick(invoice, "status")),
					})
				}
			}
		}

		return &searchOutput{
			Summary: fmt.Sprintf("%d match(es) for %q.", len(results), in.Query),
			Results: results,
		}, nil
	})

	register(r, toolSpec{
		Name:     "fetch",
		Title:    "Fetch one thing by id",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "Returns the full record for an id from search. The same data the matching get tool returns, and subject to the same permissions.",
	}, func(ctx context.Context, deps *Deps, call *Call, in fetchInput) (*fetchOutput, error) {
		kind, id, found := strings.Cut(in.Id, ":")
		if !found || id == "" {
			return nil, fmt.Errorf("id has to look like type:identifier, for example app:0f1e..., not %q", in.Id)
		}
		var path string
		switch kind {
		case "project":
			path = "/paas/projects/" + url.PathEscape(id)
		case "app":
			path = "/paas/apps/" + url.PathEscape(id)
		case "database":
			path = "/dbaas/instances/" + url.PathEscape(id)
		case "invoice":
			path = "/billing/invoices/" + url.PathEscape(id)
		default:
			return nil, fmt.Errorf("%q is not a type this server knows; use one of project, app, database or invoice", kind)
		}
		item, err := apiGet(ctx, deps, call, path, nil)
		if err != nil {
			return nil, err
		}
		// Secrets have their own tools that say so; a generic fetch must not route round them.
		item = omit(item, "envVars", "password", "connectionString", "uri", "dsn")
		encoded, _ := json.Marshal(item)
		return &fetchOutput{
			Id:       in.Id,
			Title:    pick(item, "name", "invoiceNumber", "id"),
			Text:     string(encoded),
			Metadata: item,
		}, nil
	})
}

// matches reports whether an object looks like a hit for the query. An empty
// query matches everything, which makes search usable as "show me the account".
func matches(needle string, obj map[string]any) bool {
	if needle == "" {
		return true
	}
	for _, key := range []string{"name", "id", "status", "slug", "subdomain", "invoiceNumber", "regionCode", "description"} {
		if strings.Contains(strings.ToLower(pick(obj, key)), needle) {
			return true
		}
	}
	return false
}
