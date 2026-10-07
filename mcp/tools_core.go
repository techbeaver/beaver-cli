package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/scopes"
)

// Helpers every tool group uses.

// apiGet performs a read and decodes an object.
func apiGet(ctx context.Context, deps *Deps, call *Call, path string, query url.Values) (map[string]any, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{Method: http.MethodGet, Path: path, Query: query})
	if err != nil {
		return nil, describeAPIError(err)
	}
	return client.DecodeObject(env)
}

// apiList performs a read and decodes a list.
func apiList(ctx context.Context, deps *Deps, call *Call, path string, query url.Values) ([]map[string]any, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{Method: http.MethodGet, Path: path, Query: query})
	if err != nil {
		return nil, describeAPIError(err)
	}
	return client.DecodeList(env)
}

// apiSend performs a write.
func apiSend(ctx context.Context, deps *Deps, call *Call, method, path string, body any, headers map[string]string) (*client.Envelope, error) {
	env, err := deps.Client.Do(ctx, call.Session.Token, client.Request{
		Method: method, Path: path, Body: body, Headers: headers,
	})
	if err != nil {
		return nil, describeAPIError(err)
	}
	return env, nil
}

// idempotencyHeaders builds the header a charge-creating call must carry.
//
// The key is the caller's, not ours, and that is deliberate. Generating one per
// call here would defeat the purpose entirely: the retry would carry a
// different key and produce a second charge. The agent has to reuse the same
// value when it retries, so the tool asks for it and says why.
func idempotencyHeaders(key string) map[string]string {
	return map[string]string{"Idempotency-Key": strings.TrimSpace(key)}
}

// requireIdempotencyKey refuses a money call with no key rather than inventing
// one.
func requireIdempotencyKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("this call can create a charge, so it needs an idempotency_key: any unique string you generate once and reuse if you have to retry. Do not generate a new one on retry, or the customer will be charged twice")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

type emptyInput struct{}

type whoamiOutput struct {
	Summary        string   `json:"summary" jsonschema:"one line describing who this connection acts for"`
	UserID         string   `json:"userId" jsonschema:"the TechBeaver account this connection acts for"`
	Email          string   `json:"email" jsonschema:"the account owner's email address"`
	OrganisationID string   `json:"organisationId,omitempty" jsonschema:"the organisation the account belongs to, if any"`
	ClientName     string   `json:"clientName,omitempty" jsonschema:"which AI client the customer connected"`
	Scopes         []string `json:"scopes" jsonschema:"exactly what this connection was granted; anything not listed here you cannot do"`
	CanDelete      bool     `json:"canDelete" jsonschema:"whether this connection may ask to delete things at all; even when true, every deletion still needs the account owner to approve it in a browser"`
	CanPay         bool     `json:"canPay" jsonschema:"whether this connection may prepare payments"`
	CanSpendCredit bool     `json:"canSpendCredit" jsonschema:"whether this connection can pay with no payment page (prepaid balance, saved card, credit); true only for a partner key holding billing:spend"`
	// SpendCapMinor and SpentThisMonthMinor bound what a spending connection may still pay.
	SpendCapMinor       int64    `json:"spendCapMinor,omitempty" jsonschema:"the most this connection may pay without a payment page in a calendar month, in kobo"`
	SpentThisMonthMinor int64    `json:"spentThisMonthMinor,omitempty" jsonschema:"what it has paid that way this month, in kobo"`
	Limits              []string `json:"limits" jsonschema:"the things this connection can never do, whatever it is asked"`
}

func registerIdentityTools(r *Registry) {
	register(r, toolSpec{
		Name:     "whoami",
		Title:    "Who am I acting for",
		ReadOnly: true, Idempotent: true,
		Description: "The first thing to call. Returns the TechBeaver account this connection acts for and exactly what it was granted, so you never have to guess whether an action is permitted. It also states plainly what this connection can never do.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*whoamiOutput, error) {
		id := call.Session.Identity
		name := id.Email
		if id.ClientName != "" {
			name = fmt.Sprintf("%s, connected as %s", id.Email, id.ClientName)
		}
		return &whoamiOutput{
			Summary:             fmt.Sprintf("Acting for %s.", name),
			UserID:              id.UserID,
			Email:               id.Email,
			OrganisationID:      id.OrganisationID,
			ClientName:          id.ClientName,
			Scopes:              id.Scopes,
			CanDelete:           id.Has(scopes.PaaSDestroy) || id.Has(scopes.DBaaSDestroy),
			CanPay:              id.Has(scopes.BillingCheckout),
			CanSpendCredit:      id.CanSpendWithoutCheckout,
			SpendCapMinor:       id.SpendCapMinor,
			SpentThisMonthMinor: id.SpentThisMonthMinor,
			Limits:              whoamiLimits(id),
		}, nil
	})

	register(r, toolSpec{
		Name:     "check_service_status",
		Title:    "Check whether anything is paused",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "Whether TechBeaver has paused any of its creation flows for maintenance. Worth checking before telling a customer that creating something failed: a paused flow is a deliberate operator decision with a message attached, not an error to retry.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/service-gates", nil)
		if err != nil {
			return nil, err
		}
		paused := 0
		for _, item := range items {
			if blocked, ok := item["blocked"].(bool); ok && blocked {
				paused++
			}
		}
		summary := "Nothing is paused."
		if paused > 0 {
			summary = fmt.Sprintf("%d service flow(s) are currently paused. Read the message on each and repeat it to the customer.", paused)
		}
		return &ListResult{Summary: summary, Count: len(items), Items: items}, nil
	})
}

// ---------------------------------------------------------------------------
// Projects, regions and plans
// ---------------------------------------------------------------------------

type createProjectInput struct {
	Name        string `json:"name" jsonschema:"a name for the project, 3 to 50 characters"`
	Description string `json:"description,omitempty" jsonschema:"an optional description"`
	RegionCode  string `json:"regionCode" jsonschema:"which region to host it in; call list_regions first and use a code from there, never one you remember"`
}

type projectIDInput struct {
	ProjectID string `json:"projectId" jsonschema:"the project's id, from list_projects"`
}

func registerProjectTools(r *Registry) {
	register(r, toolSpec{
		Name:     "list_regions",
		Title:    "List regions",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "The regions a project can actually be hosted in right now. Always call this before creating a project. The list is reconciled against which clusters are live, so it is the only trustworthy source: a region you remember may not exist, and a region that exists may not be active.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/paas/regions", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{
			Summary: fmt.Sprintf("%d region(s) available.", len(items)),
			Count:   len(items), Items: items,
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_projects",
		Title:    "List projects",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "Every project on this account. Apps and managed databases both live inside a project, so this is usually where to start.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/paas/projects", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{
			Summary: fmt.Sprintf("%d project(s) on this account.", len(items)),
			Count:   len(items), Items: items,
		}, nil
	})

	register(r, toolSpec{
		Name:     "get_project",
		Title:    "Get a project",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "One project's details: its region, when it was created, and who owns it. Use list_projects first if you do not already have an id.",
	}, func(ctx context.Context, deps *Deps, call *Call, in projectIDInput) (*ObjectResult, error) {
		item, err := apiGet(ctx, deps, call, "/paas/projects/"+url.PathEscape(in.ProjectID), nil)
		if err != nil {
			return nil, err
		}
		return &ObjectResult{Summary: fmt.Sprintf("Project %s.", pick(item, "name", "id")), Item: item}, nil
	})

	register(r, toolSpec{
		Name:        "create_project",
		Title:       "Create a project",
		Scopes:      []string{scopes.ProjectsWrite},
		Description: "Creates a project to hold apps and managed databases. Call list_regions first: regionCode has to be one that is active right now.",
	}, func(ctx context.Context, deps *Deps, call *Call, in createProjectInput) (*ActionResult, error) {
		env, err := apiSend(ctx, deps, call, http.MethodPost, "/paas/projects", map[string]any{
			"name":        in.Name,
			"description": in.Description,
			"regionCode":  in.RegionCode,
		}, nil)
		if err != nil {
			return nil, err
		}
		item, _ := client.DecodeObject(env)
		return &ActionResult{
			Summary: fmt.Sprintf("Created project %q in %s.", in.Name, in.RegionCode),
			Result:  item,
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_apps",
		Title:    "List apps in a project",
		Scopes:   []string{scopes.PaaSRead},
		ReadOnly: true, Idempotent: true,
		Description: "The applications deployed in one project.",
	}, func(ctx context.Context, deps *Deps, call *Call, in projectIDInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/paas/projects/"+url.PathEscape(in.ProjectID)+"/apps", nil)
		if err != nil {
			return nil, err
		}
		return &ListResult{
			Summary: fmt.Sprintf("%d app(s) in this project.", len(items)),
			Count:   len(items), Items: items,
		}, nil
	})
}

func registerPlanTools(r *Registry) {
	register(r, toolSpec{
		Name:     "list_plans",
		Title:    "List application plans",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "The application plans that can be bought right now, with their live prices. Never quote a price from memory: plans are operator-editable, can be retired, and a plan priced at zero is the free tier. Use quote_purchase for what something will actually cost, because tax and discounts are worked out server side.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/paas/plans", nil)
		if err != nil {
			return nil, err
		}
		for i := range items {
			// Enforced by nothing, so a model shown it would refuse to create a fourth app.
			items[i] = omit(items[i], "maxAppsPerProject")
		}
		return &ListResult{
			Summary: fmt.Sprintf("%d application plan(s) available. Prices here are live; do not recall them later.", len(items)),
			Count:   len(items), Items: items,
		}, nil
	})

	register(r, toolSpec{
		Name:     "list_database_plans",
		Title:    "List managed database plans",
		Scopes:   []string{scopes.ProjectsRead},
		ReadOnly: true, Idempotent: true,
		Description: "The managed PostgreSQL plans that can be bought right now. If this comes back empty, no database plan has been activated and priced, so a managed database cannot currently be bought at all: say that rather than guessing at a plan id.",
	}, func(ctx context.Context, deps *Deps, call *Call, _ emptyInput) (*ListResult, error) {
		items, err := apiList(ctx, deps, call, "/dbaas/plans", nil)
		if err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("%d managed database plan(s) available.", len(items))
		if len(items) == 0 {
			summary = "No managed database plans are available to buy at the moment."
		}
		return &ListResult{Summary: summary, Count: len(items), Items: items}, nil
	})
}

// whoamiLimits is what this connection can never do, stated so it is true for this connection.
// A partner key holding billing:spend can pay without a payment page, within its monthly cap;
// telling it otherwise would be as wrong as telling any other connection it can.
func whoamiLimits(id *Identity) []string {
	payment := "You cannot complete a payment. Checkout produces a link only the customer can pay on."
	credit := "You cannot withdraw money or spend account credit."
	if id != nil && id.CanSpendWithoutCheckout {
		payment = fmt.Sprintf("You can pay without a payment page only with paymentSource \"balance\" or \"saved_card\", and only up to this key's monthly limit (%d kobo, %d used this month). Do it only when the account owner's own system asked for that purchase.", id.SpendCapMinor, id.SpentThisMonthMinor)
		credit = "You cannot withdraw money. Credit and the prepaid balance can only be spent on TechBeaver, within the same monthly limit."
	}
	return []string{
		payment,
		credit,
		"You cannot reach anything administrative.",
		"You cannot delete anything without the account owner approving that exact deletion in their browser.",
	}
}
