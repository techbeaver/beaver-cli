package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/techbeaver/beaver-cli/scopes"
)

// Extensions on one logical database. See ADR 0017.
func registerExtensionTools(r *Registry) {
	register(r, toolSpec{
		Name:     "list_db_extensions",
		Title:    "List database extensions",
		Scopes:   []string{scopes.DBaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The extensions TechBeaver offers for one logical database, each marked with whether this instance can install it and whether it already has. Read this before enabling anything: an extension has to ship in the image behind the instance, and one marked unavailable cannot be installed on it at all.",
	}, func(ctx context.Context, deps *Deps, call *Call, in extensionListInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, extensionsPath(in.InstanceID, in.DatabaseID), nil)
		if err != nil {
			return nil, err
		}
		installed, available := 0, 0
		for _, item := range items {
			if truthy(item["installed"]) {
				installed++
			}
			if truthy(item["available"]) {
				available++
			}
		}
		return &ListResult{
			Summary: fmt.Sprintf("%d extension(s) offered, %d installable on this instance, %d already installed.",
				len(items), available, installed),
			Count: len(items),
			Items: items,
		}, nil
	})

	register(r, toolSpec{
		Name:        "enable_db_extension",
		Title:       "Enable a database extension",
		Scopes:      []string{scopes.DBaaSWrite},
		Idempotent:  true,
		Description: "Turns on one extension inside a logical database. TechBeaver runs this with the privileges the customer's own database role does not have, which is why an extension that fails with \"permission denied, must be superuser\" over a direct connection succeeds here. Dependencies are pulled in automatically. Use list_db_extensions first for the exact name.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
		DatabaseID string `json:"databaseId" jsonschema:"the logical database's id, from list_logical_databases"`
		Name       string `json:"name" jsonschema:"the extension's name exactly as list_db_extensions gives it, for example postgis or vector"`
	}) (*ActionResult, error) {
		if _, err := apiSend(ctx, deps, call, http.MethodPost,
			extensionsPath(in.InstanceID, in.DatabaseID), map[string]any{"name": in.Name}, nil); err != nil {
			return nil, err
		}
		return &ActionResult{
			Summary:  fmt.Sprintf("Enabled %q.", in.Name),
			NextStep: "It is available to every role on that database now. A migration that runs CREATE EXTENSION IF NOT EXISTS for it will pass from here on.",
		}, nil
	})

	register(r, toolSpec{
		Name:        "disable_db_extension",
		Title:       "Disable a database extension",
		Scopes:      []string{scopes.DBaaSWrite},
		Destructive: true,
		Description: "Turns one extension off again. It will be refused, harmlessly, if anything in the schema still depends on it: nothing is dropped along with it. Removing an extension a running application uses will break that application, so confirm with the customer first.",
	}, func(ctx context.Context, deps *Deps, call *Call, in struct {
		InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
		DatabaseID string `json:"databaseId" jsonschema:"the logical database's id, from list_logical_databases"`
		Name       string `json:"name" jsonschema:"the extension's name, from list_db_extensions"`
	}) (*ActionResult, error) {
		if _, err := apiSend(ctx, deps, call, http.MethodDelete,
			extensionsPath(in.InstanceID, in.DatabaseID)+"/"+url.PathEscape(in.Name), nil, nil); err != nil {
			return nil, err
		}
		return &ActionResult{Summary: fmt.Sprintf("Disabled %q.", in.Name)}, nil
	})
}

type extensionListInput struct {
	InstanceID string `json:"instanceId" jsonschema:"the managed database's id"`
	DatabaseID string `json:"databaseId" jsonschema:"the logical database's id, from list_logical_databases"`
}

func extensionsPath(instanceID, databaseID string) string {
	return "/dbaas/instances/" + url.PathEscape(instanceID) +
		"/databases/" + url.PathEscape(databaseID) + "/extensions"
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
