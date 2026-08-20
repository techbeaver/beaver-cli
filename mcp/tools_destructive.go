package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/techbeaver/beaver-cli/scopes"
)

// The tools that tear something down.
//
// They exist because an agent that can create but not clean up leaves debris
// only the console can remove, which is its own operational problem. What makes
// them safe to ship is not restraint on the agent's part: it is that the API
// refuses every one of these calls unless the account owner has approved that
// exact action in a browser. See destructive.go.
//
// Every preview here is assembled from a live read, never from an assumption,
// and every recovery note states a real window. Where this platform's recovery
// windows are stated below they come from what the code actually does, and if
// that changes these strings are wrong and have to change with it.

type deleteAppInput struct {
	AppID             string `json:"appId" jsonschema:"the app's id"`
	ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"leave this out on the first call to get a preview and an approval link; set it once the account owner has approved"`
}

func registerDestructiveTools(r *Registry) {
	register(r, toolSpec{
		Name:        "delete_app",
		Title:       "Delete an app",
		Scopes:      []string{scopes.PaaSDestroy},
		Destructive: true,
		Description: "Deletes an application. Call it WITHOUT a confirmation token first: you get a preview of what goes and a link the account owner must open in their browser. It cannot be completed any other way. If the customer only wants the app to stop running and stop costing them, use stop_app instead: that is reversible and needs no approval.",
	}, func(ctx context.Context, deps *Deps, call *Call, in deleteAppInput) (*DestructiveResult, error) {
		path := "/paas/apps/" + url.PathEscape(in.AppID)
		spec := destructiveSpec{
			Method: http.MethodDelete, Path: path,
			Action: "delete", ResourceType: "app", ResourceID: in.AppID,
			RecoveryNote: "The app record is soft deleted and its running workload is torn down. The source stays in the customer's own git repository, so the app can be recreated and redeployed from it. Any custom domain attached to it stops resolving here.",
		}
		if in.ConfirmationToken == "" {
			app, err := apiGet(ctx, deps, call, path, nil)
			if err != nil {
				return nil, err
			}
			spec.ResourceName = pick(app, "name", "id")
			domains, _ := apiList(ctx, deps, call, path+"/custom-domains", nil)
			spec.Preview = map[string]any{
				"app":                  pick(app, "name", "id"),
				"status":               pick(app, "status"),
				"url":                  pick(app, "url", "appUrl", "publicUrl"),
				"plan":                 pick(app, "planName", "paasPlanId"),
				"customDomainsRemoved": len(domains),
				"whatGoes":             "the running workload and the app's configuration, including its environment variables",
				"whatSurvives":         "the source in the customer's git repository, and any invoices already raised",
			}
			spec.ElicitMessage = fmt.Sprintf("Delete the app %q?", spec.ResourceName)
		}
		return performDestructive(ctx, deps, call, in.ConfirmationToken, spec)
	})

	register(r, toolSpec{
		Name:        "delete_database",
		Title:       "Delete a managed database",
		Scopes:      []string{scopes.DBaaSDestroy},
		Destructive: true,
		Description: "Deletes a managed PostgreSQL instance. Call it WITHOUT a confirmation token first to get a preview and an approval link the account owner must open. Read list_backups first and tell the customer what exists to restore from, especially if the answer is nothing.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID        string `json:"instanceId" jsonschema:"the managed database's id"`
		ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"leave this out on the first call to get a preview and an approval link; set it once the account owner has approved"`
	}) (*DestructiveResult, error) {
		path := "/dbaas/instances/" + url.PathEscape(in.InstanceID)
		spec := destructiveSpec{
			Method: http.MethodDelete, Path: path,
			Action: "delete", ResourceType: "managed database", ResourceID: in.InstanceID,
			RecoveryNote: "The database cluster is torn down and stops accepting connections immediately. Its backups are retained for a period after deletion, and restoring from them needs TechBeaver support: it is not something this connection or the console can do. Anything not in a backup is gone.",
		}
		if in.ConfirmationToken == "" {
			instance, err := apiGet(ctx, deps, call, path, nil)
			if err != nil {
				return nil, err
			}
			spec.ResourceName = pick(instance, "name", "id")
			backups, _ := apiList(ctx, deps, call, path+"/backups", nil)
			databases, _ := apiList(ctx, deps, call, path+"/databases", nil)
			latest := ""
			if len(backups) > 0 {
				latest = pick(backups[len(backups)-1], "completedAt", "createdAt")
			}
			spec.Preview = map[string]any{
				"database":           pick(instance, "name", "id"),
				"status":             pick(instance, "status"),
				"logicalDatabases":   len(databases),
				"backupsRecorded":    len(backups),
				"mostRecentBackup":   latest,
				"whatGoes":           "the running cluster, every logical database inside it, every role, and every live connection",
				"whatSurvives":       "backups already taken, for a retention window after deletion",
				"restoreIsSelfServe": false,
			}
			if len(backups) == 0 {
				spec.Preview["warning"] = "No backups are recorded for this database. If it is deleted there may be nothing to restore from. Say this to the customer before they approve."
			}
			spec.ElicitMessage = fmt.Sprintf("Delete the managed database %q?", spec.ResourceName)
		}
		return performDestructive(ctx, deps, call, in.ConfirmationToken, spec)
	})

	register(r, toolSpec{
		Name:        "delete_logical_database",
		Title:       "Delete a database inside an instance",
		Scopes:      []string{scopes.DBaaSDestroy},
		Destructive: true,
		Description: "Drops one logical database inside a managed instance, and everything in it. The instance itself and its other databases are untouched. Needs the account owner's approval in a browser like every other deletion.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID        string `json:"instanceId" jsonschema:"the managed database's id"`
		DatabaseID        string `json:"databaseId" jsonschema:"the logical database's id, from list_logical_databases"`
		ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"leave this out on the first call to get a preview and an approval link"`
	}) (*DestructiveResult, error) {
		path := "/dbaas/instances/" + url.PathEscape(in.InstanceID) + "/databases/" + url.PathEscape(in.DatabaseID)
		spec := destructiveSpec{
			Method: http.MethodDelete, Path: path,
			Action: "delete", ResourceType: "logical database", ResourceID: in.DatabaseID,
			RecoveryNote: "The database and everything in it is dropped. It can only come back from a backup of the whole instance, which needs TechBeaver support.",
		}
		if in.ConfirmationToken == "" {
			databases, err := apiList(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/databases", nil)
			if err != nil {
				return nil, err
			}
			for _, db := range databases {
				if pick(db, "id") == in.DatabaseID {
					spec.ResourceName = pick(db, "name", "id")
					spec.Preview = map[string]any{
						"database":     pick(db, "name", "id"),
						"owner":        pick(db, "ownerRole", "owner"),
						"whatGoes":     "this database and every table in it",
						"whatSurvives": "the instance itself and its other databases",
					}
					break
				}
			}
			if spec.Preview == nil {
				return nil, fmt.Errorf("no logical database with id %q on that instance. Call list_logical_databases and use an id from it", in.DatabaseID)
			}
			spec.ElicitMessage = fmt.Sprintf("Drop the database %q?", spec.ResourceName)
		}
		return performDestructive(ctx, deps, call, in.ConfirmationToken, spec)
	})

	register(r, toolSpec{
		Name:        "delete_project",
		Title:       "Delete a project",
		Scopes:      []string{scopes.ProjectsWrite, scopes.PaaSDestroy},
		Destructive: true,
		Description: "Deletes a project. This is the widest destructive action on this surface: a project holds apps and managed databases, so deleting it takes them with it. Needs both write and destroy permission, and the account owner's approval in a browser.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ProjectID         string `json:"projectId" jsonschema:"the project's id"`
		ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"leave this out on the first call to get a preview and an approval link"`
	}) (*DestructiveResult, error) {
		path := "/paas/projects/" + url.PathEscape(in.ProjectID)
		spec := destructiveSpec{
			Method: http.MethodDelete, Path: path,
			Action: "delete", ResourceType: "project", ResourceID: in.ProjectID,
			RecoveryNote: "Everything inside the project goes with it. Application source stays in the customer's git repositories; managed database contents do not, beyond whatever backups exist.",
		}
		if in.ConfirmationToken == "" {
			project, err := apiGet(ctx, deps, call, path, nil)
			if err != nil {
				return nil, err
			}
			apps, _ := apiList(ctx, deps, call, path+"/apps", nil)
			databases, _ := apiList(ctx, deps, call, "/dbaas/projects/"+url.PathEscape(in.ProjectID)+"/instances", nil)
			spec.ResourceName = pick(project, "name", "id")
			spec.Preview = map[string]any{
				"project":           pick(project, "name", "id"),
				"region":            pick(project, "regionCode"),
				"appsAffected":      len(apps),
				"databasesAffected": len(databases),
				"whatGoes":          "the project and everything in it",
			}
			spec.ElicitMessage = fmt.Sprintf("Delete the project %q and everything in it?", spec.ResourceName)
		}
		return performDestructive(ctx, deps, call, in.ConfirmationToken, spec)
	})

	register(r, toolSpec{
		Name:        "remove_custom_domain",
		Title:       "Remove a custom domain",
		Scopes:      []string{scopes.PaaSDestroy},
		Destructive: true,
		Description: "Detaches a customer's own domain from an app. The site stops answering on that address immediately, which is why it needs approval. The domain itself, at the registrar, is untouched.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		AppID             string `json:"appId" jsonschema:"the app's id"`
		DomainID          string `json:"domainId" jsonschema:"the domain's id, from list_custom_domains"`
		ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"leave this out on the first call to get a preview and an approval link"`
	}) (*DestructiveResult, error) {
		path := "/paas/apps/" + url.PathEscape(in.AppID) + "/custom-domains/" + url.PathEscape(in.DomainID)
		spec := destructiveSpec{
			Method: http.MethodDelete, Path: path,
			Action: "remove", ResourceType: "custom domain", ResourceID: in.DomainID,
			RecoveryNote: "The domain can be added back with add_custom_domain, but its DNS records have to be set up and verified again, so the site is unreachable on that address until they are.",
		}
		if in.ConfirmationToken == "" {
			domains, err := apiList(ctx, deps, call, "/paas/apps/"+url.PathEscape(in.AppID)+"/custom-domains", nil)
			if err != nil {
				return nil, err
			}
			for _, d := range domains {
				if pick(d, "id") == in.DomainID {
					spec.ResourceName = pick(d, "domain", "name", "id")
					spec.Preview = map[string]any{
						"domain":   spec.ResourceName,
						"status":   pick(d, "status", "verificationStatus"),
						"whatGoes": "traffic on this address stops reaching the app",
					}
					break
				}
			}
			if spec.Preview == nil {
				return nil, fmt.Errorf("no custom domain with id %q on that app. Call list_custom_domains and use an id from it", in.DomainID)
			}
			spec.ElicitMessage = fmt.Sprintf("Take %s off this app?", spec.ResourceName)
		}
		return performDestructive(ctx, deps, call, in.ConfirmationToken, spec)
	})

	register(r, toolSpec{
		Name:        "restore_database",
		Title:       "Restore a managed database",
		Scopes:      []string{scopes.DBaaSDestroy},
		Destructive: true,
		Description: "Recovers a managed database from its backups. Filed with the destructive tools deliberately: a restore overwrites what is there now, which is a deletion wearing a friendlier name. Needs the account owner's approval like any other. Read get_restore_preview first so they know what point they are going back to.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID        string `json:"instanceId" jsonschema:"the managed database to recover from"`
		Name              string `json:"name,omitempty" jsonschema:"a name for the recovered instance"`
		TargetTime        string `json:"targetTime,omitempty" jsonschema:"an RFC 3339 timestamp to recover to; omitted means the latest available point"`
		ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"leave this out on the first call to get a preview and an approval link"`
	}) (*DestructiveResult, error) {
		path := "/dbaas/instances/" + url.PathEscape(in.InstanceID) + "/restore"
		body := map[string]any{}
		putIfSet(body, "name", in.Name)
		if strings.TrimSpace(in.TargetTime) != "" {
			if _, err := time.Parse(time.RFC3339, in.TargetTime); err != nil {
				return nil, fmt.Errorf("targetTime has to be an RFC 3339 timestamp such as 2026-08-19T14:00:00Z, not %q", in.TargetTime)
			}
			body["targetTime"] = in.TargetTime
		}
		spec := destructiveSpec{
			Method: http.MethodPost, Path: path, Body: body,
			Action: "restore", ResourceType: "managed database", ResourceID: in.InstanceID,
			RecoveryNote: "A restore recovers to a point in the past. Anything written after that point is not in the recovered data. Take a backup first if the current state matters at all.",
		}
		if in.ConfirmationToken == "" {
			preview, err := apiGet(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/restore-preview", nil)
			if err != nil {
				return nil, err
			}
			instance, _ := apiGet(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID), nil)
			spec.ResourceName = pick(instance, "name", "id")
			spec.Preview = map[string]any{
				"database":      spec.ResourceName,
				"restoreWindow": preview,
				"targetTime":    in.TargetTime,
				"whatHappens":   "data is recovered to the chosen point; anything written after it will not be there",
			}
			spec.ElicitMessage = fmt.Sprintf("Restore the database %q?", spec.ResourceName)
		}
		return performDestructive(ctx, deps, call, in.ConfirmationToken, spec)
	})

	register(r, toolSpec{
		Name:     "get_restore_preview",
		Title:    "See what a restore would recover",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "What a restore could recover and how far back it can go. Reads only, changes nothing, and is the right thing to look at before proposing a restore.",
	}, func(ctx context.Context, deps *Deps, call *Call, in instanceIDInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/dbaas/instances/"+url.PathEscape(in.InstanceID)+"/restore-preview", nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{Summary: "What a restore could recover.", Item: item}, nil
	})
}

// registerCleanupTool adds the dry run.
func registerCleanupTool(r *Registry) {
	register(r, toolSpec{
		Name:     "plan_cleanup",
		Title:    "Find things worth cleaning up",
		Scopes:   []string{scopes.PaaSRead, scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "A dry run. Looks across a project, or the whole account, and lists what LOOKS unused: apps that are stopped or never deployed, provisioning that failed. It removes nothing and it cannot remove anything. Show the customer the list, let them choose, and then call the relevant delete tool for each one they pick, which will still need their approval in a browser.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		ProjectID string `json:"projectId,omitempty" jsonschema:"limit the scan to one project; omitted means every project on the account"`
	}) (*ListResult, error) {
		projects, err := scopeProjects(ctx, deps, call, in.ProjectID)
		if err != nil {
			return nil, err
		}

		var candidates []map[string]any
		for _, project := range projects {
			projectID := pick(project, "id")
			if projectID == "" {
				continue
			}
			apps, _ := apiList(ctx, deps, call, "/paas/projects/"+url.PathEscape(projectID)+"/apps", nil)
			for _, app := range apps {
				if reason := idleAppReason(app); reason != "" {
					candidates = append(candidates, map[string]any{
						"kind":       "app",
						"id":         pick(app, "id"),
						"name":       pick(app, "name"),
						"project":    pick(project, "name", "id"),
						"status":     pick(app, "status"),
						"reason":     reason,
						"removeWith": "delete_app",
					})
				}
			}
			databases, _ := apiList(ctx, deps, call, "/dbaas/projects/"+url.PathEscape(projectID)+"/instances", nil)
			for _, db := range databases {
				if reason := idleDatabaseReason(db); reason != "" {
					candidates = append(candidates, map[string]any{
						"kind":       "database",
						"id":         pick(db, "id"),
						"name":       pick(db, "name"),
						"project":    pick(project, "name", "id"),
						"status":     pick(db, "status"),
						"reason":     reason,
						"removeWith": "delete_database",
					})
				}
			}
		}

		summary := fmt.Sprintf("%d thing(s) look unused. Nothing has been removed.", len(candidates))
		if len(candidates) == 0 {
			summary = "Nothing looks unused."
		}
		return &ListResult{Summary: summary, Count: len(candidates), Items: candidates}, nil
	})
}

// scopeProjects resolves the cleanup scan's scope.
func scopeProjects(ctx context.Context, deps *Deps, call *Call, projectID string) ([]map[string]any, error) {
	if projectID != "" {
		project, err := apiGet(ctx, deps, call, "/paas/projects/"+url.PathEscape(projectID), nil)
		if err != nil {
			return nil, err
		}
		return []map[string]any{project}, nil
	}
	return apiList(ctx, deps, call, "/paas/projects", nil)
}

// idleAppReason says why an app looks unused, or returns "".
//
// Deliberately conservative, and it says LOOKS. A stopped app is very often
// stopped on purpose, and an agent that presents this list as "safe to delete"
// rather than "worth asking about" is the failure mode this tool has to avoid.
func idleAppReason(app map[string]any) string {
	status := strings.ToLower(pick(app, "status"))
	live, _ := app["hasLiveRevision"].(bool)
	switch {
	case status == "failed":
		return "its last deployment failed"
	case status == "created" && !live:
		return "it was created but has never been deployed"
	case status == "stopped":
		return "it is stopped, so it is not serving anything"
	}
	return ""
}

// idleDatabaseReason says why a managed database looks unused, or returns "".
func idleDatabaseReason(db map[string]any) string {
	switch strings.ToLower(pick(db, "status")) {
	case "failed":
		return "provisioning failed, so it never became usable"
	case "suspended":
		return "it is suspended"
	}
	return ""
}
