package apigw

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// openAPIDoc is the subset of the document these tests reason about.
type openAPIDoc struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Tags []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"tags"`
	Security   []map[string][]string           `json:"security"`
	Paths      map[string]map[string]openAPIOp `json:"paths"`
	Components struct {
		SecuritySchemes map[string]map[string]any `json:"securitySchemes"`
		Parameters      map[string]openAPIParam   `json:"parameters"`
		Responses       map[string]any            `json:"responses"`
		Schemas         map[string]any            `json:"schemas"`
	} `json:"components"`
}

type openAPIOp struct {
	OperationID string              `json:"operationId"`
	Summary     string              `json:"summary"`
	Description string              `json:"description"`
	Tags        []string            `json:"tags"`
	Security    *[]map[string][]any `json:"security"`
	Parameters  []openAPIParam      `json:"parameters"`
	RequestBody *struct {
		Required bool           `json:"required"`
		Content  map[string]any `json:"content"`
	} `json:"requestBody"`
	Responses map[string]any `json:"responses"`
}

type openAPIParam struct {
	Ref      string `json:"$ref"`
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	var doc openAPIDoc
	if err := json.Unmarshal(OpenAPIDocument(), &doc); err != nil {
		t.Fatalf("the embedded OpenAPI document is not valid JSON: %v", err)
	}
	return doc
}

// httpMethods are the operation keys a path item may carry.
var httpMethods = []string{"get", "put", "post", "delete", "patch", "options", "head", "trace"}

func isMethodKey(k string) bool {
	for _, m := range httpMethods {
		if k == m {
			return true
		}
	}
	return false
}

// TestOpenAPIMatchesRouteTable is the agreement check.
//
// The document is written by hand — a generated one would guarantee agreement
// and describe nothing — so something has to notice when the two drift. This
// walks both and fails on any disagreement about which paths and methods
// exist, what each operation is called, which path parameters it takes,
// whether it has a request body, and whether it needs a credential.
func TestOpenAPIMatchesRouteTable(t *testing.T) {
	t.Parallel()
	doc := loadOpenAPI(t)

	documented := map[string]openAPIOp{}
	for path, item := range doc.Paths {
		for method, op := range item {
			if !isMethodKey(method) {
				t.Errorf("path %s has a non-method key %q", path, method)
				continue
			}
			documented[strings.ToUpper(method)+" "+path] = op
		}
	}

	registered := map[string]Route{}
	for _, rt := range Routes() {
		registered[rt.Key()] = rt
	}

	// Every route is documented.
	for key, rt := range registered {
		op, ok := documented[key]
		if !ok {
			t.Errorf("route %s is registered but not documented in openapi.json", key)
			continue
		}
		if op.OperationID != rt.Operation {
			t.Errorf("%s: operationId %q, route says %q", key, op.OperationID, rt.Operation)
		}
		if op.Summary != rt.Summary {
			t.Errorf("%s: summary drifted\n  document: %q\n  route:    %q", key, op.Summary, rt.Summary)
		}
		if len(rt.Tags) > 0 && (len(op.Tags) == 0 || op.Tags[0] != rt.Tags[0]) {
			t.Errorf("%s: tags %v, route says %v", key, op.Tags, rt.Tags)
		}

		// Authentication: a public route declares an empty security list,
		// which is how OpenAPI says "this one needs no credential"; an
		// authenticated route inherits the document-level requirement and must
		// not override it.
		switch {
		case rt.Public && (op.Security == nil || len(*op.Security) != 0):
			t.Errorf("%s is public but does not declare `\"security\": []`", key)
		case !rt.Public && op.Security != nil:
			t.Errorf("%s overrides the document-level security requirement", key)
		}

		// Request bodies.
		hasBody := op.RequestBody != nil
		if rt.NoBody && hasBody {
			t.Errorf("%s takes no request body but documents one", key)
		}
		if !rt.NoBody && !hasBody {
			t.Errorf("%s takes a request body but does not document one", key)
		}

		// Path parameters.
		wantParams := pathWildcards(rt.Pattern)
		gotParams := map[string]bool{}
		for _, p := range op.Parameters {
			name := p.Name
			if p.Ref != "" {
				name = strings.TrimPrefix(p.Ref, "#/components/parameters/")
				resolved, ok := doc.Components.Parameters[name]
				if !ok {
					t.Errorf("%s: parameter $ref %q does not resolve", key, p.Ref)
					continue
				}
				if resolved.In != "path" {
					continue
				}
				name = resolved.Name
				if !resolved.Required {
					t.Errorf("%s: path parameter %q is not marked required", key, name)
				}
			} else if p.In != "path" {
				continue
			}
			gotParams[name] = true
		}
		for _, want := range wantParams {
			if !gotParams[want] {
				t.Errorf("%s: path parameter %q is in the route pattern but not documented", key, want)
			}
		}
		for got := range gotParams {
			if !contains(wantParams, got) {
				t.Errorf("%s: documents path parameter %q which the route does not have", key, got)
			}
		}

		if len(op.Responses) == 0 {
			t.Errorf("%s documents no responses", key)
		}
	}

	// And nothing is documented that is not routed.
	for key := range documented {
		if _, ok := registered[key]; !ok {
			t.Errorf("openapi.json documents %s, which no route serves", key)
		}
	}

	if len(registered) != len(documented) {
		t.Errorf("%d routes, %d documented operations", len(registered), len(documented))
	}
}

func TestOpenAPIDocumentIsWellFormed(t *testing.T) {
	t.Parallel()
	doc := loadOpenAPI(t)

	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("openapi version %q, want 3.1.x", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		t.Fatal("info.title and info.version are required")
	}
	if len(doc.Servers) == 0 {
		t.Error("no servers listed; a client cannot construct a URL from this document")
	}
	if len(doc.Security) == 0 {
		t.Error("the document declares no default security requirement")
	}
	for _, scheme := range []string{"apiKey", "bearerAuth", "mutualTLS"} {
		if _, ok := doc.Components.SecuritySchemes[scheme]; !ok {
			t.Errorf("security scheme %q is not described", scheme)
		}
	}
	// Every tag an operation uses must be described, or a rendered reference
	// shows a bare slug with no explanation.
	described := map[string]bool{}
	for _, tag := range doc.Tags {
		described[tag.Name] = true
		if tag.Description == "" {
			t.Errorf("tag %q has no description", tag.Name)
		}
	}
	for _, rt := range Routes() {
		for _, tag := range rt.Tags {
			if !described[tag] {
				t.Errorf("route %s uses tag %q, which the document does not describe", rt.Key(), tag)
			}
		}
	}
	// The document names its own error contract; clients branch on it.
	if _, ok := doc.Components.Schemas["Error"]; !ok {
		t.Error("no Error schema is defined")
	}
}

func TestOpenAPIRefsResolve(t *testing.T) {
	t.Parallel()
	var raw map[string]any
	if err := json.Unmarshal(OpenAPIDocument(), &raw); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	var refs []string
	collectRefs(raw, &refs)
	if len(refs) == 0 {
		t.Fatal("the document contains no $ref at all, which is not the document that was written")
	}
	sort.Strings(refs)
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if !strings.HasPrefix(ref, "#/") {
			t.Errorf("$ref %q is not a local pointer; the document must be self-contained", ref)
			continue
		}
		if resolvePointer(raw, ref) == nil {
			t.Errorf("$ref %q does not resolve", ref)
		}
	}
}

func TestOpenAPIIsServedAndRevalidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet, "/openapi.json", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag; a console left open across a deployment would re-download the document")
	}
	var doc map[string]any
	decodeBody(t, res, &doc)
	if doc["openapi"] == nil {
		t.Fatal("the served document is not an OpenAPI document")
	}

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/openapi.json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("If-None-Match", etag)
	res2, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation returned %d, want 304", res2.StatusCode)
	}
}

func TestDocsPageIsServedAndSelfContained(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodGet, "/docs", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	body := bodyString(t, res)
	if !strings.Contains(body, "/openapi.json") {
		t.Error("the viewer does not fetch the document it is meant to render")
	}
	assertNoExternalResources(t, body)
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q; the page must not be framable", csp)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// pathWildcards extracts the {name} wildcards from a route pattern.
func pathWildcards(pattern string) []string {
	var out []string
	rest := pattern
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return out
		}
		shut := strings.IndexByte(rest[open:], '}')
		if shut < 0 {
			return out
		}
		out = append(out, rest[open+1:open+shut])
		rest = rest[open+shut+1:]
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func collectRefs(node any, out *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "$ref" {
				if s, ok := child.(string); ok {
					*out = append(*out, s)
				}
				continue
			}
			collectRefs(child, out)
		}
	case []any:
		for _, child := range v {
			collectRefs(child, out)
		}
	}
}

func resolvePointer(root any, ref string) any {
	cur := root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

// assertNoExternalResources checks that an embedded page loads nothing from
// off-origin. A console that needs a CDN is a console that does not render
// during the store-network incident it exists for.
func assertNoExternalResources(t *testing.T, body string) {
	t.Helper()
	for _, needle := range []string{"http://", "https://", "//cdn", "integrity="} {
		if strings.Contains(body, needle) {
			t.Errorf("the page references %q; embedded pages must be entirely self-contained", needle)
		}
	}
}
