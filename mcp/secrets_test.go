package mcp

import (
	"testing"
)

/*
The bug this file exists for.

get_connection_info called omit(item, "password", "connectionString", "uri", "dsn", "url") and
reported "Connection details, without the password". The payload it was filtering does not carry a
field called any of those things except password: the connection strings are directUri, internalUri
and pooledUri, and each one has the password inside it. All three were returned in full, under a
summary saying the opposite, and a live customer database password reached a chat transcript.

So the tests below are written against the real field names from
internal/dbaas/service/connection.go rather than against the filter's own vocabulary, and the
family case (a connection string under a field name nobody has thought of yet) is asserted
directly. A test that only checked "password" is gone would have passed on the broken code.
*/

// connectionPayload mirrors the JSON shape of dbaas ConnectionInfo, with the field names taken from
// the struct tags in internal/dbaas/service/connection.go. The password here is fictional.
func connectionPayload() map[string]any {
	return map[string]any{
		"databaseId":   "0de0f74e-9af8-4a8e-b2a8-eec488456ea6",
		"databaseName": "engodb",
		"username":     "engo_used",
		"password":     "fake-not-a-real-password",
		"directHost":   "example.db.techbeaver.io",
		"directPort":   float64(30001),
		"sslMode":      "require",
		"directUri":    "postgresql://engo_used:fake-not-a-real-password@example.db.techbeaver.io:30001/engodb?sslmode=require",
		"internalHost": "example.internal.db.techbeaver.io",
		"internalUri":  "postgresql://engo_used:fake-not-a-real-password@example.internal.db.techbeaver.io:30001/engodb?sslmode=require",
		"pooledHost":   "example.db.techbeaver.io",
		"pooledPort":   float64(30002),
		"pooledUri":    "postgresql://engo_used:fake-not-a-real-password@example.db.techbeaver.io:30002/engodb?sslmode=require",
	}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestOmitSecretsRemovesEveryConnectionString(t *testing.T) {
	got := omitSecrets(connectionPayload())

	for _, gone := range []string{"password", "directUri", "internalUri", "pooledUri"} {
		assert.NotContains(t, got, gone,
			"%s carries the password and must never leave the server without includeSecrets", gone)
	}

	for _, kept := range []string{"databaseId", "databaseName", "username", "directHost", "directPort", "sslMode", "internalHost", "pooledHost", "pooledPort"} {
		assert.Contains(t, got, kept, "%s is not a secret and the caller needs it", kept)
	}
}

func TestOmitSecretsLeavesNoPasswordAnywhereInTheOutput(t *testing.T) {
	const password = "fake-not-a-real-password"
	for key, value := range omitSecrets(connectionPayload()) {
		str, ok := value.(string)
		if !ok {
			continue
		}
		assert.NotContains(t, str, password, "the password survived inside %q", key)
	}
}

// ---------------------------------------------------------------------------
// The family, not the instance
// ---------------------------------------------------------------------------

func TestOmitSecretsCatchesAConnectionStringNobodyNamed(t *testing.T) {
	got := omitSecrets(map[string]any{
		"replicaUri":     "postgresql://u:pw@replica.db.techbeaver.io:30003/db",
		"somethingElse":  "mysql://root:hunter2@db.internal:3306/app",
		"harmlessDetail": "keep me",
	})

	assert.NotContains(t, got, "replicaUri")
	assert.NotContains(t, got, "somethingElse")
	assert.Contains(t, got, "harmlessDetail")
}

func TestOmitSecretsCatchesSecretsByNameWhateverTheyHold(t *testing.T) {
	got := omitSecrets(map[string]any{
		"apiToken":         "abc123",
		"clientSecret":     "shhh",
		"dbPassword":       "pw",
		"dsn":              "host=x user=y password=z",
		"connectionString": "Server=x;Password=y",
		"awsCredential":    "AKIA...",
		"name":             "engo-prod-db",
	})

	for _, gone := range []string{"apiToken", "clientSecret", "dbPassword", "dsn", "connectionString", "awsCredential"} {
		assert.NotContains(t, got, gone)
	}
	assert.Contains(t, got, "name")
}

func TestOmitSecretsKeepsAPlainAppURL(t *testing.T) {
	got := omitSecrets(map[string]any{
		"url":    "https://engo-battery-rental-r2wx06.techbeaver.app",
		"gitUrl": "https://github.com/techbeaver/engo-api.git",
		"status": "deployed",
	})

	assert.Contains(t, got, "url", "a public app URL is not a credential")
	assert.Contains(t, got, "gitUrl")
	assert.Contains(t, got, "status")
}

// ---------------------------------------------------------------------------
// Boundaries and zero values
// ---------------------------------------------------------------------------

func TestOmitSecretsBoundaries(t *testing.T) {
	t.Run("empty map", func(t *testing.T) {
		assert.Empty(t, omitSecrets(map[string]any{}))
	})

	t.Run("nil map", func(t *testing.T) {
		assert.Empty(t, omitSecrets(nil))
	})

	t.Run("empty string value", func(t *testing.T) {
		got := omitSecrets(map[string]any{"directUri": ""})
		assert.Contains(t, got, "directUri", "an empty string carries no password")
	})

	t.Run("non-string values are untouched", func(t *testing.T) {
		got := omitSecrets(map[string]any{
			"directPort": float64(30001),
			"autoRenew":  true,
			"nested":     map[string]any{"a": 1},
			"list":       []any{"a", "b"},
			"nilValue":   nil,
		})
		assert.Len(t, got, 5)
	})

	t.Run("userinfo with no password is kept", func(t *testing.T) {
		got := omitSecrets(map[string]any{"repo": "https://git@github.com/techbeaver/engo-api.git"})
		assert.Contains(t, got, "repo", "a username alone is not a credential")
	})

	t.Run("a bare string that is not a URI is kept", func(t *testing.T) {
		got := omitSecrets(map[string]any{"note": "user:pass@host, but no scheme"})
		assert.Contains(t, got, "note")
	})
}

// ---------------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------------

func TestOmitSecretsFailsClosedOnAnUnparseableURI(t *testing.T) {
	got := omitSecrets(map[string]any{
		"directUri": "postgresql://user:pw@[::1",
		"keepMe":    "plain",
	})

	assert.NotContains(t, got, "directUri", "an unverifiable URI must not be returned")
	assert.Contains(t, got, "keepMe")
}

func TestOmitSecretsDoesNotPanicOnOddInput(t *testing.T) {
	require.NotPanics(t, func() {
		omitSecrets(map[string]any{
			"a": func() {},
			"b": make(chan int),
			"c": "postgresql://u:p@h/d",
		})
	})
}

// ---------------------------------------------------------------------------
// The caller's map is theirs
// ---------------------------------------------------------------------------

func TestOmitSecretsDoesNotMutateItsInput(t *testing.T) {
	in := connectionPayload()
	before := len(in)

	_ = omitSecrets(in)

	assert.Len(t, in, before, "the caller's map was modified")
	assert.Contains(t, in, "password")
	assert.Contains(t, in, "directUri")
}

func TestOmitSecretsAlsoDropsNamedKeys(t *testing.T) {
	got := omitSecrets(map[string]any{
		"envVars": map[string]any{"APP_DATABASE_URL": "..."},
		"name":    "Engo Backend API",
	}, "envVars")

	assert.NotContains(t, got, "envVars")
	assert.Contains(t, got, "name")
}
