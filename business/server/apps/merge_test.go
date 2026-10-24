package apps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestMergeDefaults verifies that minimal app input produces a complete stored
// configuration rather than leaving defaults to change between renderings.
func TestMergeDefaults(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"frontend","image":"ghcr.io/example/frontend:v1","port":3000}]}`))
	assert.NoErr(err)
	assert.Equal(len(configuration.Apps), 1)
	application := configuration.Apps[0]
	assert.Equal(application.Replicas, 1)
	assert.Equal(application.Env, EnvReference{Version: "latest"})
	assert.Equal(application.Ingresses, []Ingress{})
	assert.Equal(application.Command, "")
	assert.Equal(application.Predeploy, Predeploy{TimeoutSeconds: 300})
	assert.Equal(application.Checks.Readiness, Check{Enabled: true, Path: "/healthz", Port: 3000, PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3})
	assert.Equal(application.Checks.Startup, application.Checks.Readiness)
	encoded, err := json.Marshal(configuration)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), ":null"))
}

// TestMergeRetainsOmittedState checks the patch contract across apps, named
// lists, explicit zero values, ordered arguments, and independent healthchecks.
func TestMergeRetainsOmittedState(t *testing.T) {
	assert := is.New(t)
	current, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"frontend","image":"frontend:v1","port":3000,"env":{"name":"backend-env","version":"7"},"ingresses":[{"name":"public","host":"example.com"},{"name":"internal","host":"internal.example.com"}],"command":"node server.js --debug"},{"name":"backend","image":"backend:v1","port":8080}]}`))
	assert.NoErr(err)
	before, err := json.Marshal(current)
	assert.NoErr(err)
	merged, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"frontend","replicas":0,"env":{"name":"backend-env","version":"7"},"ingresses":[{"name":"public","host":"new.example.com","path":"/app"}],"command":"node server.js","checks":{"readiness":{"enabled":false},"startup":{"path":"/started","port":9000,"periodSeconds":2,"timeoutSeconds":5,"failureThreshold":90}}}]}`))
	assert.NoErr(err)
	assert.Equal(len(merged.Apps), 2)
	assert.Equal(merged.Apps[0].Name, "backend")
	application := merged.Apps[1]
	assert.Equal(application.Image, "frontend:v1")
	assert.Equal(application.Replicas, 0)
	assert.Equal(application.Env, EnvReference{Name: "backend-env", Version: "7"})
	assert.Equal(application.Ingresses, []Ingress{{Name: "internal", Host: "internal.example.com", Path: "/"}, {Name: "public", Host: "new.example.com", Path: "/app"}})
	assert.Equal(application.Command, "node server.js")
	assert.Equal(application.Checks.Readiness.Enabled, false)
	assert.Equal(application.Checks.Startup, Check{Enabled: true, Path: "/started", Port: 9000, PeriodSeconds: 2, TimeoutSeconds: 5, FailureThreshold: 90})
	after, err := json.Marshal(current)
	assert.NoErr(err)
	assert.Equal(string(after), string(before))
}

// TestMergeEmptyLists distinguishes omission from explicit empty lists, so API
// clients can clear fields without accidentally deleting unmentioned entries.
func TestMergeEmptyLists(t *testing.T) {
	assert := is.New(t)
	current, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"frontend","image":"frontend:v1","port":3000,"env":{"name":"backend-env","version":"7"},"ingresses":[{"name":"public","host":"example.com"}],"command":"node server.js"}]}`))
	assert.NoErr(err)
	unchanged, err := Merge(current, Patch{})
	assert.NoErr(err)
	assert.Equal(unchanged, current)
	cleared, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"frontend","env":{"version":"0"},"ingresses":[],"command":""}]}`))
	assert.NoErr(err)
	assert.Equal(len(cleared.Apps), 1)
	assert.Equal(cleared.Apps[0].Env, EnvReference{Name: "backend-env", Version: "0"})
	assert.Equal(cleared.Apps[0].Ingresses, []Ingress{})
	assert.Equal(cleared.Apps[0].Command, "")
	empty, err := Merge(current, patchFromJson(t, `{"apps":[]}`))
	assert.NoErr(err)
	assert.Equal(empty.Apps, []App{})
	initial, err := Merge(Configuration{}, Patch{})
	assert.NoErr(err)
	assert.Equal(initial.Apps, []App{})
}

// TestMergeEnvironmentReferences preserves omission and pinned versions while
// any supplied env object without a version selects latest for the next plan.
func TestMergeEnvironmentReferences(t *testing.T) {
	assert := is.New(t)
	current, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"old","version":"7"}}]}`))
	assert.NoErr(err)
	unchanged, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v2"}]}`))
	assert.NoErr(err)
	assert.Equal(unchanged.Apps[0].Env, current.Apps[0].Env)
	for _, env := range []string{`{}`, `{"name":"old"}`} {
		updated, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","env":`+env+`}]}`))
		assert.NoErr(err)
		assert.Equal(updated.Apps[0].Env, EnvReference{Name: "old", Version: "latest"})
	}
	switched, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","env":{"name":"next"}}]}`))
	assert.NoErr(err)
	assert.Equal(switched.Apps[0].Env, EnvReference{Name: "next", Version: "latest"})
	latest, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","env":{"version":"latest"}}]}`))
	assert.NoErr(err)
	assert.Equal(latest.Apps[0].Env, EnvReference{Name: "old", Version: "latest"})
	pinned, err := Merge(switched, patchFromJson(t, `{"apps":[{"name":"backend","env":{"version":"3"}}]}`))
	assert.NoErr(err)
	assert.Equal(pinned.Apps[0].Env, EnvReference{Name: "next", Version: "3"})
}

// TestMergeNoListener checks that private workers can omit networking and that
// explicit probe overrides remain available even without a Service listener.
func TestMergeNoListener(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"worker","image":"example/worker:v1"}]}`))
	assert.NoErr(err)
	assert.Equal(configuration.Apps[0].Port, 0)
	assert.Equal(configuration.Apps[0].Checks.Readiness.Enabled, false)
	assert.Equal(configuration.Apps[0].Checks.Startup.Enabled, false)
	updated, err := Merge(configuration, patchFromJson(t, `{"apps":[{"name":"worker","checks":{"readiness":{"enabled":true,"port":9090,"path":"/ready"},"startup":{"enabled":true,"port":9090}}}]}`))
	assert.NoErr(err)
	assert.Equal(updated.Apps[0].Checks.Readiness.Enabled, true)
	assert.Equal(updated.Apps[0].Checks.Readiness.Port, 9090)
	assert.Equal(updated.Apps[0].Checks.Readiness.Path, "/ready")
}

// TestMergePartialChecks preserves startup overrides when readiness alone is
// updated, and treats an empty checks object as an omitted update.
func TestMergePartialChecks(t *testing.T) {
	assert := is.New(t)
	current, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"checks":{"startup":{"path":"/started","failureThreshold":30}}}]}`))
	assert.NoErr(err)
	updated, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","checks":{"readiness":{"path":"/ready"}}}]}`))
	assert.NoErr(err)
	assert.Equal(updated.Apps[0].Checks.Readiness.Path, "/ready")
	assert.Equal(updated.Apps[0].Checks.Startup, current.Apps[0].Checks.Startup)
	empty, err := Merge(updated, patchFromJson(t, `{"apps":[{"name":"backend","checks":{}}]}`))
	assert.NoErr(err)
	assert.Equal(empty, updated)
}

// TestMergeRejectsInvalidInput ensures invalid updates fail before reaching
// chart rendering or Kubernetes, including ambiguous duplicate list entries.
func TestMergeRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  string
	}{
		{"missing image", `{"apps":[{"name":"frontend"}]}`, "image is required"},
		{"malformed command", `{"apps":[{"name":"app","image":"app:v1","command":"/app \"unterminated"}]}`, "command"},
		{"invalid image", `{"apps":[{"name":"frontend","image":"not an image"}]}`, "invalid image"},
		{"invalid name", `{"apps":[{"name":"Uppercase","image":"app:v1"}]}`, "name must be"},
		{"long app name", `{"apps":[{"name":"` + strings.Repeat("a", 51) + `","image":"app:v1"}]}`, "name must be at most 50 characters"},
		{"duplicate apps", `{"apps":[{"name":"app","image":"app:v1"},{"name":"app","image":"app:v2"}]}`, "duplicate app"},
		{"negative port", `{"apps":[{"name":"app","image":"app:v1","port":-1}]}`, "port must be"},
		{"large port", `{"apps":[{"name":"app","image":"app:v1","port":65536}]}`, "port must be"},
		{"negative replicas", `{"apps":[{"name":"app","image":"app:v1","replicas":-1}]}`, "replicas must be"},
		{"large replicas", `{"apps":[{"name":"app","image":"app:v1","replicas":2147483648}]}`, "replicas must be"},
		{"invalid environment", `{"apps":[{"name":"app","image":"app:v1","env":{"name":"invalid/name","version":"1"}}]}`, "group name"},
		{"invalid environment version", `{"apps":[{"name":"app","image":"app:v1","env":{"name":"backend-env","version":"01"}}]}`, "group version"},
		{"duplicate ingresses", `{"apps":[{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"public","host":"a.com"},{"name":"public","host":"b.com"}]}]}`, "duplicate ingress"},
		{"route without listener", `{"apps":[{"name":"app","image":"app:v1","ingresses":[{"name":"public","host":"example.com"}]}]}`, "nonzero application port"},
		{"invalid ingress name", `{"apps":[{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"invalid/name","host":"example.com"}]}]}`, "invalid ingress name"},
		{"invalid ingress host", `{"apps":[{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"public","host":"https://example.com"}]}]}`, "invalid ingress host"},
		{"IP ingress host", `{"apps":[{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"public","host":"127.0.0.1"}]}]}`, "invalid ingress host"},
		{"invalid ingress path", `{"apps":[{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"public","host":"example.com","path":"relative"}]}]}`, "absolute HTTP path"},
		{"probe without port", `{"apps":[{"name":"app","image":"app:v1","checks":{"readiness":{"enabled":true}}}]}`, "probe port"},
		{"invalid probe path", `{"apps":[{"name":"app","image":"app:v1","checks":{"readiness":{"path":"/healthz?bad"}}}]}`, "absolute HTTP path"},
		{"invalid probe budget", `{"apps":[{"name":"app","image":"app:v1","checks":{"startup":{"timeoutSeconds":0}}}]}`, "must be positive"},
		{"overflowing probe budget", `{"apps":[{"name":"app","image":"app:v1","checks":{"startup":{"periodSeconds":2147483648}}}]}`, "integer fields"},
		{"ambiguous ingress resources", `{"apps":[{"name":"app-public","image":"app:v1","port":80,"ingresses":[{"name":"http","host":"a.example.com"}]},{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"public-http","host":"b.example.com"}]}]}`, "ingress resource"},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			assert := is.New(t)
			_, err := Merge(Configuration{}, patchFromJson(t, scenario.patch))
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), scenario.want))
		})
	}
}

// TestMergeStoredIntegrity guards against silently losing entries when stored
// data contains duplicate names, and accepts wildcard ingress hosts.
func TestMergeStoredIntegrity(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"app","image":"app:v1","port":80,"ingresses":[{"name":"public","host":"*.example.com"}]}]}`))
	assert.NoErr(err)
	duplicateApp := Configuration{Apps: []App{configuration.Apps[0], configuration.Apps[0]}}
	_, err = Merge(duplicateApp, Patch{})
	assert.True(err != nil)
	duplicateVariables := cloneApp(configuration.Apps[0])
	duplicateVariables.Env = EnvReference{Name: "invalid/name", Version: "1"}
	_, err = Merge(Configuration{Apps: []App{duplicateVariables}}, Patch{})
	assert.True(err != nil)
	duplicateIngresses := cloneApp(configuration.Apps[0])
	duplicateIngresses.Ingresses = append(duplicateIngresses.Ingresses, duplicateIngresses.Ingresses[0])
	_, err = Merge(Configuration{Apps: []App{duplicateIngresses}}, Patch{})
	assert.True(err != nil)
}

func patchFromJson(t *testing.T, document string) Patch {
	t.Helper()
	patch := Patch{}
	if err := json.Unmarshal([]byte(document), &patch); err != nil {
		t.Fatal(err)
	}
	return patch
}
