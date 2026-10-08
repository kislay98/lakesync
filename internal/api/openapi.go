package api

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"lakesync/internal/core"
)

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// openapi generates the OpenAPI 3.0 document from the registered routes, so
// the paths and methods can never drift from the handlers.
func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, 200, s.openapiDoc())
}

func (s *Server) openapiDoc() map[string]any {
	paths := map[string]map[string]any{}
	tags := map[string]bool{}
	for _, rt := range s.routes {
		op := map[string]any{
			"summary":     rt.Summary,
			"tags":        []string{rt.Tag},
			"operationId": operationID(rt.Method, rt.Path),
			"responses": map[string]any{
				"200":     map[string]any{"description": "Success"},
				"default": map[string]any{"description": "Error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}},
			},
		}
		tags[rt.Tag] = true
		var params []map[string]any
		for _, m := range pathParam.FindAllStringSubmatch(rt.Path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		if rt.Path == "/api/tables/{ns}/{table}/rows" {
			for _, q := range []string{"limit", "cursor", "sort", "filter", "q", "columns", "destination", "total"} {
				params = append(params, map[string]any{"name": q, "in": "query", "schema": map[string]any{"type": "string"}})
			}
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if rt.Auth {
			op["security"] = []map[string]any{{"cookieAuth": []string{}}}
		}
		if rt.Method == "POST" || rt.Method == "PUT" {
			op["requestBody"] = map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
		}
		if rt.Path == "/api/events" {
			op["responses"].(map[string]any)["200"] = map[string]any{"description": "text/event-stream of JSON events (types: pipeline, collection, run, log, maintenance)"}
		}
		if paths[rt.Path] == nil {
			paths[rt.Path] = map[string]any{}
		}
		paths[rt.Path][strings.ToLower(rt.Method)] = op
	}
	var tagList []map[string]string
	for t := range tags {
		tagList = append(tagList, map[string]string{"name": t})
	}
	sort.Slice(tagList, func(i, j int) bool { return tagList[i]["name"] < tagList[j]["name"] })
	return map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": core.AppName + " API", "version": s.cfg.Version, "description": "JSON API of " + core.AppName + ". Errors always have the shape {\"error\": {\"code\", \"message\", \"hint\"}}."},
		"servers": []map[string]any{{"url": "/"}},
		"tags":    tagList,
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{"cookieAuth": map[string]any{"type": "apiKey", "in": "cookie", "name": sessionCookie, "description": "Session cookie set by POST /api/auth/login. State-changing requests also need the X-CSRF-Token header from GET /api/me."}},
			"schemas": map[string]any{
				"Error": map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "object", "properties": map[string]any{
					"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "hint": map[string]any{"type": "string"}}}}},
			},
		},
	}
}

func operationID(method, path string) string {
	p := strings.TrimPrefix(path, "/api/")
	p = pathParam.ReplaceAllString(p, "by_$1")
	p = strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(p)
	return strings.ToLower(method) + "_" + strings.Trim(p, "_")
}
