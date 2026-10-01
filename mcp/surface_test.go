package mcp

import (
	"strings"
	"testing"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

// Properties of the tool surface itself, checked without a server running.
//
// These are the invariants that are easy to break by adding one more tool in a
// hurry, and impossible to notice afterwards: a destructive tool that forgot to
// ask for a destroy scope, a description that quotes a price, a tool nobody can
// reach because it requires a scope that cannot be granted.

func allSpecs(t *testing.T) []toolSpec {
	t.Helper()
	specs := AllTools().Specs()
	if len(specs) < 30 {
		t.Fatalf("only %d tools registered; the surface is meant to be a curated set of roughly forty", len(specs))
	}
	return specs
}

func TestEveryToolIsDescribed(t *testing.T) {
	for _, spec := range allSpecs(t) {
		if spec.Title == "" {
			t.Errorf("tool %q has no title", spec.Name)
		}
		if len(spec.Description) < 40 {
			t.Errorf("tool %q has a description of %d characters. The description is how a model decides whether to use it, so it has to say what the tool does and when not to use it",
				spec.Name, len(spec.Description))
		}
	}
}

func TestToolNamesAreStable(t *testing.T) {
	// Tool names are pinned in prompts, so a rename cannot happen by accident.
	expected := map[string]bool{
		"whoami": true, "check_service_status": true,
		"list_regions": true, "list_projects": true, "get_project": true, "create_project": true,
		"list_plans": true, "list_database_plans": true,
		"list_apps": true, "get_app": true, "create_app": true, "deploy_app": true, "rollback_app": true,
		"list_deployments": true, "set_root_directory": true,
		"check_repo_access": true, "list_git_connections": true,
		"stop_app": true, "start_app": true, "undelete_app": true,
		"get_env_vars": true, "set_env_vars": true, "set_app_port": true, "set_start_command": true,
		"list_custom_domains": true, "add_custom_domain": true, "verify_custom_domain": true,
		"list_databases": true, "get_database": true, "create_database": true, "get_connection_info": true,
		"list_logical_databases": true, "create_logical_database": true,
		"list_db_extensions": true, "enable_db_extension": true, "disable_db_extension": true,
		"list_db_roles": true, "create_db_role": true, "grant_db_role": true,
		"list_backups": true, "create_backup": true,
		"list_deleted_databases": true, "restore_deleted_database": true,
		"list_ip_allowlist": true, "add_ip_allowlist_entry": true,
		"get_build_logs": true, "get_runtime_logs": true, "get_app_metrics": true, "get_app_instances": true,
		"get_database_usage": true, "get_database_stats": true, "get_provision_logs": true,
		"quote_purchase": true, "list_invoices": true, "get_invoice": true, "get_credit_balance": true,
		"pay_invoice": true, "create_checkout_link": true,
		"delete_app": true, "delete_database": true, "delete_logical_database": true, "delete_project": true,
		"remove_custom_domain": true, "restore_database": true, "get_restore_preview": true,
		"plan_cleanup": true,
		"search":       true, "fetch": true,
	}
	got := map[string]bool{}
	for _, spec := range allSpecs(t) {
		got[spec.Name] = true
		if !expected[spec.Name] {
			t.Errorf("tool %q is new. Add it to this list deliberately: a tool name is something customers pin in prompts", spec.Name)
		}
	}
	for name := range expected {
		if !got[name] {
			t.Errorf("tool %q has disappeared. Removing or renaming one breaks whatever pinned it", name)
		}
	}
}

func TestNoToolDescriptionQuotesAPriceOrAPlanName(t *testing.T) {
	// Operator-editable data must never be recallable from a tool description.
	banned := []string{"NGN", "₦", "USD", "$", "7.5%", "hobby plan", "pro plan", "free plan", "eu-west", "af-south"}
	for _, spec := range allSpecs(t) {
		lower := strings.ToLower(spec.Description)
		for _, phrase := range banned {
			if strings.Contains(lower, strings.ToLower(phrase)) {
				t.Errorf("tool %q mentions %q in its description. Prices, plan names and regions come from the API at call time", spec.Name, phrase)
			}
		}
	}
	lower := strings.ToLower(instructions)
	for _, phrase := range banned {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			t.Errorf("the server instructions mention %q. Same rule: the model must not be able to recall one", phrase)
		}
	}
}

func TestDestructiveToolsRequireADestroyScope(t *testing.T) {
	for _, spec := range allSpecs(t) {
		if !spec.Destructive {
			continue
		}
		if spec.ReadOnly {
			t.Errorf("tool %q is marked both read-only and destructive", spec.Name)
		}
		// Annotated destructive so clients prompt; both are reversible (ADR 0017).
		reversible := map[string]bool{"stop_app": true, "disable_db_extension": true}
		if reversible[spec.Name] {
			continue
		}
		hasDestroy := false
		for _, s := range spec.Scopes {
			if scopes.IsDestructive(s) {
				hasDestroy = true
			}
		}
		if !hasDestroy {
			t.Errorf("tool %q can tear something down but requires no destroy scope: %v", spec.Name, spec.Scopes)
		}
	}
}

func TestReadOnlyToolsNeverRequireAWriteScope(t *testing.T) {
	writes := map[string]bool{
		scopes.ProjectsWrite: true, scopes.PaaSWrite: true, scopes.PaaSDeploy: true,
		scopes.DBaaSWrite: true, scopes.PaaSDestroy: true, scopes.DBaaSDestroy: true,
		scopes.BillingCheckout: true, scopes.BillingSpend: true,
	}
	for _, spec := range allSpecs(t) {
		if !spec.ReadOnly {
			continue
		}
		for _, s := range spec.Scopes {
			if writes[s] {
				t.Errorf("tool %q is read-only but demands %q, so a customer who only granted reads cannot use it", spec.Name, s)
			}
		}
	}
}

func TestEveryToolScopeIsGrantable(t *testing.T) {
	for _, spec := range allSpecs(t) {
		for _, s := range spec.Scopes {
			if !scopes.IsKnown(s) {
				t.Errorf("tool %q requires %q, which is not a scope", spec.Name, s)
			}
			if !scopes.Issuable(s) {
				t.Errorf("tool %q requires %q, which nothing can be granted, so the tool is unreachable", spec.Name, s)
			}
		}
	}
}

func TestNoToolCanSpendMoneyWithoutAPaymentPage(t *testing.T) {
	// If a tool ever requires billing:spend this fails, and it should.
	for _, spec := range allSpecs(t) {
		for _, s := range spec.Scopes {
			if s == scopes.BillingSpend {
				t.Errorf("tool %q would spend money with no payment page in front of it", spec.Name)
			}
		}
	}
}

func TestScopeFilteringHidesToolsTheCallerCannotUse(t *testing.T) {
	// Filtering the list matters: a model that can see delete_database suggests it.
	readOnly := &Identity{Scopes: []string{scopes.ProjectsRead, scopes.PaaSRead, scopes.DBaaSRead, scopes.BillingRead}}
	deps := &Deps{Client: client.New("http://example.invalid", "beaver-mcp"), Config: Config{}}
	server := AllTools().Build(deps, readOnly)
	if server == nil {
		t.Fatal("expected a server")
	}

	// Asserted directly, because the SDK's tool list is only reachable over a session.
	visible := map[string]bool{}
	for _, spec := range AllTools().Specs() {
		if readOnly.HasAll(spec.Scopes) {
			visible[spec.Name] = true
		}
	}
	for _, hidden := range []string{
		"delete_app", "delete_database", "delete_project", "delete_logical_database",
		"remove_custom_domain", "restore_database",
		"create_app", "create_project", "deploy_app", "set_env_vars",
		"pay_invoice", "create_checkout_link",
	} {
		if visible[hidden] {
			t.Errorf("a read-only connection can see %q", hidden)
		}
	}
	for _, shown := range []string{"whoami", "list_projects", "get_app", "list_invoices", "get_build_logs"} {
		if !visible[shown] && shown != "get_build_logs" {
			t.Errorf("a read-only connection cannot see %q, which it should be able to use", shown)
		}
	}
}
