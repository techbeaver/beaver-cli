package mcp

// AllTools assembles the complete tool surface.
//
// The order of the groups is the order a customer would meet them: who am I,
// what have I got, change something, watch it, pay for it, tidy up. It has no
// effect on behaviour, and the tests assert the properties that do.
func AllTools() *Registry {
	r := &Registry{}
	registerIdentityTools(r)
	registerProjectTools(r)
	registerPlanTools(r)
	registerGitTools(r)
	registerAppTools(r)
	registerDatabaseTools(r)
	registerObservationTools(r)
	registerBillingTools(r)
	registerDestructiveTools(r)
	registerCleanupTool(r)
	registerSearchTools(r)
	return r
}
