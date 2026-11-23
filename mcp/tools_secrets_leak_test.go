package mcp

import (
	"net/http"
	"strings"
	"testing"
)

const leakPassword = "fake-not-a-real-password"

func leakyHandler() http.Handler {
	connection := connectionPayload()
	connection["logicalDatabases"] = []any{map[string]any{
		"name": "engodb",
		"uris": map[string]any{"primary": "postgresql://engo_used:" + leakPassword + "@h.example.test:30001/engodb"},
	}}
	connection["extra"] = map[string]any{"replicaUri": "postgres://u:" + leakPassword + "@r.example.test/db"}
	withSecret := func() map[string]any {
		m := connectionPayload()
		m["id"], m["name"] = "inst-1", "engo"
		return m
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case path == "/mcp/whoami":
			ok(w, map[string]any{"userId": "u1", "email": "a@example.test", "principal": "machine",
				"scopes": []string{"dbaas:read", "dbaas:write", "projects:read"}})
		case strings.HasSuffix(path, "/connection-info"):
			ok(w, connection)
		case strings.HasSuffix(path, "/roles"), strings.HasSuffix(path, "/databases") && r.Method == http.MethodPost:
			ok(w, withSecret())
		case strings.HasPrefix(path, "/dbaas/instances/"):
			ok(w, withSecret())
		default:
			fail(w, http.StatusNotFound, "not_found", "no such thing")
		}
	})
}

func TestToolsThatShowDatabasesNeverReturnACredentialInAnyField(t *testing.T) {
	session := connectToHandler(t, leakyHandler())
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"get_connection_info", map[string]any{"instanceId": "i", "databaseId": "d"}},
		{"fetch", map[string]any{"id": "database:inst-1"}},
		{"create_logical_database", map[string]any{"instanceId": "i", "name": "n"}},
		{"create_db_role", map[string]any{"instanceId": "i", "name": "n", "password": "x-not-real-1"}},
	}
	for _, c := range calls {
		res := callTool(t, session, c.tool, c.args)
		text := resultText(res)
		if res.IsError {
			t.Fatalf("%s failed: %s", c.tool, text)
		}
		if strings.Contains(text, leakPassword) {
			t.Errorf("%s returned the database password: %s", c.tool, text)
		}
		for _, field := range []string{"directUri", "internalUri", "pooledUri", "replicaUri", "primary"} {
			if strings.Contains(text, field) {
				t.Errorf("%s returned the credential-bearing field %q", c.tool, field)
			}
		}
		if c.tool == "get_connection_info" && !strings.Contains(text, "directHost") {
			t.Errorf("get_connection_info lost the host it exists to give: %s", text)
		}
	}
}
