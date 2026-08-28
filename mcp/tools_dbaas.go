package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

// Managed PostgreSQL: creating instances, the databases and roles inside them,
// backups, and what they are doing.
//
// Deleting anything, and restoring over live data, live in
// tools_destructive.go behind the human gate. A restore is filed there
// deliberately: it overwrites data, which is a deletion wearing a friendlier
// name.

type instanceIDInput struct {
	InstanceID string `json:"instanceId" jsonschema:"the managed database's id, from list_databases"`
}

func registerDatabaseTools(r *Registry) {
	register(r, toolSpec{
		Name:     "list_databases",
		Title:    "List managed databases",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The managed PostgreSQL instances in a project.",
	}, func(ctx context.Context, deps *Deps, call *Call, in projectIDInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/dbaas/projects/"+url.PathEscape(in.ProjectID)+"/instances", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{Summary: fmt.Sprintf("%d managed database(s).", len(items)), Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:     "get_database",
		Title:    "Get a managed database",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "One managed database's status, plan, version and endpoint.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID), nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{
			Summary: fmt.Sprintf("Database %s is %s.", pick(item, "name", "id"), pick(item, "status")),
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:        "create_database",
		Title:       "Create a managed database",
		Scopes:      []string{scopes.DBaaSWrite},
		Description: "Provisions a managed PostgreSQL instance in a project. Call list_database_plans first and use a plan id from it. Provisioning is asynchronous and can take minutes: poll get_database rather than creating a second one.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ProjectID      string `json:"projectId" jsonschema:"the project to create it in"`
		Name           string `json:"name" jsonschema:"a name for the database instance"`
		PlanID         string `json:"planId" jsonschema:"which plan to buy, from list_database_plans"`
		BillingCycle   string `json:"billingCycle,omitempty" jsonschema:"monthly or yearly"`
		DatabaseName   string `json:"databaseName,omitempty" jsonschema:"the name of the first database inside the instance"`
		Username       string `json:"username,omitempty" jsonschema:"the login role that will own it"`
		IdempotencyKey string `json:"idempotencyKey" jsonschema:"a unique string you generate once; reuse exactly the same value if you retry, or the customer may be charged twice"`
	}) (*ActionResult, error) {
		if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
			return nil, err
		}
		body := map[string]any{"name": in.Name, "dbaasPlanId": in.PlanID}
		putIfSet(body, "billingCycle", in.BillingCycle)
		putIfSet(body, "databaseName", in.DatabaseName)
		putIfSet(body, "username", in.Username)

		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/dbaas/projects/"+url.PathEscape(in.ProjectID)+"/instances", body, idempotencyHeaders(in.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  fmt.Sprintf("Creating managed database %q.", in.Name),
			Result:   item,
			NextStep: "Provisioning takes a few minutes. Poll get_database, and if it is on a priced plan use create_checkout_link so the customer can pay.",
		}, nil
	})

	register(r, toolSpec{
		Name:     "get_connection_info",
		Title:    "Get connection details",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "How to connect to a logical database: host, port, database name and user. The password is withheld unless you explicitly ask for it. Ask only when the customer asked for the password itself, and tell them it will appear in this conversation.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID     string `json:"instanceId" jsonschema:"the managed database's id"`
		DatabaseID     string `json:"databaseId" jsonschema:"the logical database's id, from list_logical_databases"`
		IncludeSecrets bool   `json:"includeSecrets,omitempty" jsonschema:"set true ONLY if the customer asked for the password; it will appear in this conversation"`
	}) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call,
			"/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/databases/"+url.PathEscape(in.DatabaseID)+"/connection-info", nil)
		if err != nil {
			return nil, err
		}
		if !in.IncludeSecrets {
			// A connection string embeds the password, so it goes too.
			item = omit(item, "password", "connectionString", "uri", "dsn", "url")
			return &ObjectResult{
				Summary: "Connection details, without the password. Ask again with includeSecrets if the customer wants it.",
				Item:    item,
			}, nil
		}
		return &ObjectResult{
			Summary: "Connection details including the password. Tell the customer this is now in the conversation and should be rotated if that is a problem.",
			Item:    item,
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_logical_databases",
		Title:    "List databases inside an instance",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The logical databases inside one managed PostgreSQL instance.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/databases", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{Summary: fmt.Sprintf("%d logical database(s).", len(items)), Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:        "create_logical_database",
		Title:       "Create a database inside an instance",
		Scopes:      []string{scopes.DBaaSWrite},
		Description: "Creates a new logical database inside an existing managed instance, with its own owner role.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
		Name       string `json:"name" jsonschema:"the database name"`
		OwnerRole  string `json:"ownerRole,omitempty" jsonschema:"the role that should own it; one is derived if omitted"`
	}) (*ActionResult, error) {
		body := map[string]any{"name": in.Name}
		putIfSet(body, "ownerRole", in.OwnerRole)
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/databases", body, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: fmt.Sprintf("Created database %q.", in.Name), Result: omit(item, "password")}, nil
	})

	registerExtensionTools(r)

	register(r, toolSpec{
		Name:     "list_db_roles",
		Title:    "List database roles",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The login roles on a managed database instance. Passwords are never returned here, by the API, for any caller.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/roles", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{Summary: fmt.Sprintf("%d role(s).", len(items)), Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:        "create_db_role",
		Title:       "Create a database role",
		Scopes:      []string{scopes.DBaaSWrite},
		Description: "Creates a login role. The password is returned once, in this response, and cannot be read back afterwards, so pass it to the customer immediately and tell them it is in the conversation.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
		Name       string `json:"name" jsonschema:"the role name"`
		Password   string `json:"password" jsonschema:"the password for the role, at least 8 characters"`
	}) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/roles",
			map[string]any{"name": in.Name, "password": in.Password}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary:  fmt.Sprintf("Created role %q.", in.Name),
			Result:   omit(item, "password"),
			NextStep: "The password is the one you were given. It cannot be read back later, so make sure the customer has stored it.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "grant_db_role",
		Title:       "Grant a role access to a database",
		Scopes:      []string{scopes.DBaaSWrite},
		Description: "Gives a role read, write or full access to one logical database.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
		RoleID     string `json:"roleId" jsonschema:"the role's id, from list_db_roles"`
		DatabaseID string `json:"databaseId" jsonschema:"the logical database's id"`
		Level      string `json:"level" jsonschema:"read, write or all"`
	}) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/roles/"+url.PathEscape(in.RoleID)+"/grants",
			map[string]any{"databaseId": in.DatabaseID, "level": in.Level}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: fmt.Sprintf("Granted %s access.", in.Level), Result: item}, nil
	})

	register(r, toolSpec{
		Name:     "list_backups",
		Title:    "List backups",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The backups taken of a managed database. Read this before proposing anything destructive, so you can tell the customer what actually exists to restore from rather than assuming something does.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/backups", nil)
		if err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("%d backup(s).", len(items))
		if len(items) == 0 {
			summary = "No backups are recorded for this database. Say so plainly before anything destructive is considered."
		}
		return &ListResult{Summary: summary, Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:        "create_backup",
		Title:       "Take a backup",
		Scopes:      []string{scopes.DBaaSWrite},
		Description: "Starts an on-demand backup. Worth doing before any risky change, and it is the one thing on this surface that only makes recovery more likely.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/backups", map[string]any{}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: "Backup started.", Result: item}, nil
	})

	register(r, toolSpec{
		Name:     "list_ip_allowlist",
		Title:    "List allowed IP addresses",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "Which addresses may connect to a managed database. The usual explanation for a connection that times out from somewhere new.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/ip-allowlist", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{Summary: fmt.Sprintf("%d allowlist entr(ies).", len(items)), Count: len(items), Items: items}, nil
	})

	register(r, toolSpec{
		Name:        "add_ip_allowlist_entry",
		Title:       "Allow an IP address",
		Scopes:      []string{scopes.DBaaSWrite},
		Description: "Lets an address or range connect to a managed database. Prefer the narrowest range that works: an allowlist of 0.0.0.0/0 is not an allowlist.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
		CIDR       string `json:"cidr" jsonschema:"an address or CIDR range, for example 203.0.113.4 or 203.0.113.0/24"`
		Label      string `json:"label,omitempty" jsonschema:"a note about what this is, so it can be recognised later"`
	}) (*ActionResult, error) {
		body := map[string]any{"cidr": in.CIDR}
		putIfSet(body, "label", in.Label)
		env, err := apiSend(ctx, deps, call, http.MethodPost,
			"/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/ip-allowlist", body, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{Summary: fmt.Sprintf("Allowed %s.", in.CIDR), Result: item}, nil
	})
}
