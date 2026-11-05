package capture

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// pathProjectPrefix starts a base URL that names the project: /p/<project>/...
const pathProjectPrefix = "/p/"

type pathProjectKey struct{}

// PathProjectFrom returns the project that StripPathProject took from the
// request URL, or "".
func PathProjectFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	project, _ := ctx.Value(pathProjectKey{}).(string)
	return project
}

// StripPathProject lets clients that cannot send headers name the project in
// their base URL, for example https://host:8317/p/my-app/v1. It removes the
// /p/<project> prefix before routing, so every route works unchanged, and
// keeps the project in the request context for the trace middleware. An empty
// project name is answered with 400. The prefix is not removed in front of the
// management API and the control panel. It works whether traces are enabled or
// not.
func StripPathProject(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil || r.URL == nil {
			next.ServeHTTP(w, r)
			return
		}
		escaped := r.URL.EscapedPath()
		if !strings.HasPrefix(escaped, pathProjectPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		rawName, rest, _ := strings.Cut(escaped[len(pathProjectPrefix):], "/")
		project, err := url.PathUnescape(rawName)
		project = strings.TrimSpace(project)
		if err != nil || project == "" {
			http.Error(w, "project name in /p/<project> is empty or invalid", http.StatusBadRequest)
			return
		}
		remainder := "/" + rest
		if isProtectedPath(remainder) {
			next.ServeHTTP(w, r)
			return
		}
		path, err := url.PathUnescape(remainder)
		if err != nil {
			http.Error(w, "invalid path escape", http.StatusBadRequest)
			return
		}
		u := *r.URL
		u.Path = path
		u.RawPath = remainder
		stripped := r.WithContext(context.WithValue(r.Context(), pathProjectKey{}, project))
		stripped.URL = &u
		stripped.RequestURI = u.RequestURI()
		next.ServeHTTP(w, stripped)
	})
}

// isProtectedPath reports whether path belongs to the management API or the
// control panel. Those routes never take a project prefix.
func isProtectedPath(path string) bool {
	for _, base := range []string{"/v0/management", "/v8/management", "/management.html"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}
