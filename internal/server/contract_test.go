package server

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/pkg/idtoken"
)

const specPath = "../../api/openapi.yaml"

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestOpenAPI_DocumentIsValid(t *testing.T) {
	if err := loadSpec(t).Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Every real route must be documented and every documented operation must be a
// real route, so the client generated from the spec never drifts.
func TestOpenAPI_RoutesMatchDocument(t *testing.T) {
	probe := func(context.Context) error { return nil }
	// Handlers are never invoked here, so nil services are fine.
	iss := service.NewIssuer("k", time.Minute, time.Now)
	authn := middleware.Auth(iss, allowAll{})
	limit := middleware.RateLimitIP(ratelimit.NewMemory(time.Now), "login", 10, time.Minute)
	r := NewRouter(logger.New("error", &strings.Builder{}), handler.NewHealthHandler(probe, probe),
		handler.NewAuthHandler(nil, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{}).
			WithLoginLimit(limit),
		handler.NewUserHandler(nil, authn),
		handler.NewRoomHandler(nil, nil, authn).WithManage(nil),
		handler.NewCodeHandler(nil, authn),
		handler.NewConfigHandler(authn),
		handler.NewChatHandler(nil, authn),
		handler.NewWsHandler(nil, nil, nil, nil, nil, service.DefaultRealtimeConfig("0.0.0"), authn),
		handler.NewBlockHandler(nil, authn), handler.NewReportHandler(nil, authn),
		handler.NewCheckinHandler(nil, authn))

	routes := map[string]bool{}
	for _, ri := range r.Routes() {
		routes[ri.Method+" "+openAPIPath(ri.Path)] = true
	}
	docOps := map[string]bool{}
	for path, item := range loadSpec(t).Paths.Map() {
		for method := range item.Operations() {
			docOps[method+" "+path] = true
		}
	}
	missingInDoc, missingInCode := diff(routes, docOps), diff(docOps, routes)
	if len(missingInDoc) > 0 || len(missingInCode) > 0 {
		t.Fatalf("routes not in openapi.yaml: %v\noperations without a route: %v", missingInDoc, missingInCode)
	}
}

func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestOpenAPI_EveryOperationDeclaresErrorEnvelope(t *testing.T) {
	for path, item := range loadSpec(t).Paths.Map() {
		if path == "/healthz" { // liveness cannot fail
			continue
		}
		for method, op := range item.Operations() {
			found := false
			for status, ref := range op.Responses.Map() {
				if (strings.HasPrefix(status, "4") || strings.HasPrefix(status, "5")) && ref.Value != nil {
					if mt := ref.Value.Content.Get("application/json"); mt != nil && mt.Schema != nil &&
						mt.Schema.Ref == "#/components/schemas/Error" {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("%s %s declares no 4xx/5xx response with the Error schema", method, path)
			}
		}
	}
}

type allowAll struct{}

func (allowAll) AccountStatus(context.Context, string) (string, error) { return "active", nil }

// openAPIPath rewrites Gin path parameters (/:id) as OpenAPI ones (/{id}).
func openAPIPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if strings.HasPrefix(s, ":") {
			parts[i] = "{" + s[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}
