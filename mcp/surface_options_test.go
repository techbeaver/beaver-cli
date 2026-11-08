package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/techbeaver/beaver-cli/scopes"
)

func listedTools(t *testing.T, session *mcpsdk.ClientSession) map[string]*mcpsdk.Tool {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcpsdk.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func specByName(t *testing.T) map[string]toolSpec {
	t.Helper()
	out := map[string]toolSpec{}
	for _, s := range AllTools().Specs() {
		out[s.Name] = s
	}
	return out
}

// The operator's choices (read-only, scope list, secrets) are the only way to
// widen or narrow the surface. These tests walk every registered tool, so a
// tool added later is covered without anyone remembering to list it.

func TestReadOnlyOffersExactlyTheReadOnlyTools(t *testing.T) {
	session := connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{ReadOnly: true})
	listed := listedTools(t, session)
	specs := specByName(t)
	if len(listed) == 0 {
		t.Fatal("read-only mode offered nothing")
	}
	for name := range listed {
		if !specs[name].ReadOnly {
			t.Errorf("read-only mode offers %q, which is not marked read-only", name)
		}
	}
	for name, spec := range specs {
		if spec.ReadOnly && !spec.Secret && !spec.Destructive {
			if _, ok := listed[name]; !ok {
				t.Errorf("read-only mode hides %q, which only reads", name)
			}
		}
	}
	for _, write := range []string{"create_app", "set_env_vars", "deploy_app", "delete_app", "pay_invoice", "create_checkout_link", "restart_app"} {
		if _, ok := listed[write]; ok {
			t.Errorf("read-only mode offers %q", write)
		}
	}
}

func TestEveryToolOfferedUnderAScopeListNeedsOnlyThoseScopes(t *testing.T) {
	limit := []string{scopes.ProjectsRead, scopes.PaaSRead, scopes.LogsRead}
	session := connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{Scopes: limit})
	listed := listedTools(t, session)
	specs := specByName(t)
	for name := range listed {
		if missing := scopes.Missing(limit, specs[name].Scopes); len(missing) > 0 {
			t.Errorf("%q needs %v, which is outside the --scope list", name, missing)
		}
	}
	for _, want := range []string{"list_projects", "get_app", "get_build_logs"} {
		if _, ok := listed[want]; !ok {
			t.Errorf("%q needs only listed scopes and is missing", want)
		}
	}
	for _, not := range []string{"create_app", "list_databases", "list_invoices"} {
		if _, ok := listed[not]; ok {
			t.Errorf("%q is outside the scope list and was offered", not)
		}
	}
}

func TestAScopeListCannotWidenWhatTheCredentialWasGranted(t *testing.T) {
	session := connectLocalWith(t, &fakeAPI{scopes: []string{scopes.ProjectsRead}}, Surface{Scopes: scopes.All})
	for name, spec := range specByName(t) {
		if _, ok := listedTools(t, session)[name]; ok && !scopes.HasAll([]string{scopes.ProjectsRead}, spec.Scopes) {
			t.Errorf("%q was offered to a credential that lacks %v", name, spec.Scopes)
		}
	}
}

func TestSecretToolsAreOffUnlessTheOperatorAllowsThem(t *testing.T) {
	granted := &fakeAPI{scopes: scopes.All}
	off := listedTools(t, connectLocalWith(t, granted, Surface{}))
	on := listedTools(t, connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{AllowSecrets: true}))
	for name, spec := range specByName(t) {
		_, offered := off[name]
		_, offeredOn := on[name]
		switch {
		case spec.Secret && offered:
			t.Errorf("secret tool %q is offered without --allow-secrets", name)
		case spec.Secret && !offeredOn:
			t.Errorf("secret tool %q is missing with --allow-secrets and a credential holding secrets:read", name)
		}
	}
}

func TestSecretToolsNeedTheSecretsScopeEvenWhenAllowed(t *testing.T) {
	session := connectLocalWith(t, &fakeAPI{scopes: scopes.Default}, Surface{AllowSecrets: true})
	listed := listedTools(t, session)
	for name, spec := range specByName(t) {
		if spec.Secret {
			if _, ok := listed[name]; ok {
				t.Errorf("%q was offered to a credential with no secrets:read", name)
			}
		}
	}
}

func TestOnlySecretToolsMayRequireTheSecretsScope(t *testing.T) {
	for name, spec := range specByName(t) {
		needs := scopes.Has(spec.Scopes, scopes.SecretsRead)
		if spec.Secret && !needs {
			t.Errorf("secret tool %q does not require %s", name, scopes.SecretsRead)
		}
		if needs && !spec.Secret {
			t.Errorf("%q requires %s but is not marked Secret, so it would be offered without --allow-secrets", name, scopes.SecretsRead)
		}
	}
}

var secretishArgument = regexp.MustCompile(`(?i)secret|reveal|includevalue|include_value|unmask|showvalue`)

func TestNoToolLetsTheModelAskForSecrets(t *testing.T) {
	session := connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{AllowSecrets: true})
	for name, tool := range listedTools(t, session) {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Fatal(err)
		}
		for arg := range parsed.Properties {
			if secretishArgument.MatchString(arg) {
				t.Errorf("tool %q takes an argument %q the model could set to widen what it sees", name, arg)
			}
		}
	}
}

func TestReadToolsNeverAskTheAPIToReveal(t *testing.T) {
	api := &fakeAPI{scopes: scopes.All}
	session := connectLocalWith(t, api, Surface{})
	callTool(t, session, "get_env_vars", map[string]any{"appId": "app-1", "includeValues": true})
	callTool(t, session, "get_connection_info", map[string]any{"instanceId": "db-1", "databaseId": "d-1", "includeSecrets": true})
	for _, req := range api.snapshot() {
		if strings.Contains(req.Query, "reveal") {
			t.Fatalf("%s?%s asked for secrets", req.Path, req.Query)
		}
	}
}

func TestRevealToolsAskForExactlyWhatTheOperatorAllowed(t *testing.T) {
	api := &fakeAPI{scopes: scopes.All}
	session := connectLocalWith(t, api, Surface{AllowSecrets: true})
	res := callTool(t, session, "reveal_env_vars", map[string]any{"appId": "app-1"})
	if !strings.Contains(resultText(res), "hunter2") {
		t.Fatalf("reveal_env_vars withheld the values: %s", resultText(res))
	}
	var asked bool
	for _, req := range api.snapshot() {
		if req.Path == "/api/v1/paas/apps/app-1/env" && req.Query == "reveal=true" {
			asked = true
		}
	}
	if !asked {
		t.Fatal("reveal_env_vars never sent reveal=true")
	}
}

func TestSettingEnvironmentVariablesNeedsNoSecretsScopeAndNeverReads(t *testing.T) {
	api := &fakeAPI{scopes: scopes.Default}
	session := connectLocalWith(t, api, Surface{})
	res := callTool(t, session, "set_env_vars", map[string]any{"appId": "app-1", "vars": map[string]any{"A": "1"}})
	if res.IsError {
		t.Fatalf("set_env_vars without secrets:read failed: %s", resultText(res))
	}
	var patched bool
	for _, req := range api.snapshot() {
		if req.Method == "GET" && strings.HasSuffix(req.Path, "/env") {
			t.Errorf("set_env_vars read the current variables")
		}
		if strings.Contains(req.Query, "reveal") {
			t.Errorf("a write path asked for reveal")
		}
		patched = patched || (req.Method == "POST" && strings.HasSuffix(req.Path, "/env/upsert"))
		if req.Method == "PATCH" || req.Method == "PUT" {
			t.Errorf("a whole-set write was sent")
		}
	}
	if !patched {
		t.Fatal("no upsert was sent")
	}
}

func TestAnOlderAPIWithoutUpsertFailsClearlyAndNeverFallsBack(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		code   string
	}{"not allowlisted": {403, "route_not_permitted"}, "no route": {404, "upstream_not_json"}} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{scopes: scopes.All, upsertStatus: c.status, upsertCode: c.code}
			session := connectLocalWith(t, api, Surface{})
			res := callTool(t, session, "set_env_vars", map[string]any{"appId": "app-1", "vars": map[string]any{"A": "1"}})
			if !res.IsError || !strings.Contains(resultText(res), "does not support") {
				t.Fatalf("%v %s", res.IsError, resultText(res))
			}
			for _, req := range api.snapshot() {
				if req.Method == "PATCH" || req.Method == "PUT" {
					t.Fatalf("fell back to %s", req.Method)
				}
			}
		})
	}
}

func listedResources(t *testing.T, session *mcpsdk.ClientSession) map[string]bool {
	t.Helper()
	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range res.Resources {
		out[r.URI] = true
	}
	return out
}

func TestResourcesFollowTheOperatorsScopeList(t *testing.T) {
	all := listedResources(t, connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{}))
	if len(all) == 0 {
		t.Fatal("no resources are offered at all")
	}
	if got := listedResources(t, connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{Scopes: []string{scopes.PaaSRead}})); len(got) != 0 {
		t.Errorf("resources needing projects:read were offered under --scope paas:read: %v", got)
	}
	if got := listedResources(t, connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{Scopes: []string{scopes.ProjectsRead}})); len(got) != len(all) {
		t.Errorf("--scope projects:read offered %d of %d resources", len(got), len(all))
	}
	if got := listedResources(t, connectLocalWith(t, &fakeAPI{scopes: scopes.All}, Surface{ReadOnly: true})); len(got) != len(all) {
		t.Errorf("--read-only hid a resource, which only reads: %v", got)
	}
}
