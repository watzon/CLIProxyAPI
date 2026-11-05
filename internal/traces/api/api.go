// Package api serves the trace endpoints under /v8/management/traces.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
	log "github.com/sirupsen/logrus"
)

// ConfigEditor saves a value under traces.<path...> in config.yaml through the
// management config save path and returns the saved configuration.
type ConfigEditor interface {
	SetTracesConfigValue(ctx context.Context, path []string, value any) (*config.Config, error)
}

// Handler serves the trace endpoints.
type Handler struct {
	svc    *traces.Service
	editor ConfigEditor
	now    func() time.Time
}

// New creates the handler. editor may be nil, which makes config writes fail.
func New(svc *traces.Service, editor ConfigEditor) *Handler {
	return &Handler{svc: svc, editor: editor, now: time.Now}
}

// SetClock replaces the clock used for default time ranges (tests).
func (h *Handler) SetClock(now func() time.Time) { h.now = now }

// Register adds the routes to a group mounted at /v8/management.
func (h *Handler) Register(g gin.IRoutes) {
	g.GET("/traces/health", h.health)
	g.GET("/traces/stats", h.stats)
	g.GET("/traces/projects", h.projects)
	g.PUT("/traces/projects/:id", h.updateProject)
	g.GET("/traces/project-rules", h.getRules)
	g.PUT("/traces/project-rules", h.putRules)
	g.GET("/traces/sessions", h.sessions)
	g.GET("/traces/sessions/:id", h.session)
	g.GET("/traces/turns/:id", h.turn)
	g.GET("/traces/turns/:id/blocks/:index", h.block)
	g.GET("/traces/errors", h.errors)
	g.GET("/traces/limits", h.limits)
	g.PUT("/traces/limits/:account_id", h.putLimit)
	g.GET("/traces/prices", h.prices)
	g.PUT("/traces/prices", h.putPrices)
	g.GET("/traces/export.csv", h.export)
	g.DELETE("/traces", h.delete)
}

func fail(c *gin.Context, status int, code, message string) {
	body := gin.H{"error": code}
	if message != "" {
		body["message"] = message
	}
	c.JSON(status, body)
}

func internalError(c *gin.Context, err error) {
	log.WithError(err).Warn("traces: management query failed")
	fail(c, http.StatusInternalServerError, "traces_query_failed", err.Error())
}

// filter parses the common query filters. The default range is the last 30 days.
func (h *Handler) filter(c *gin.Context) (store.TurnFilter, bool) {
	to := h.now().UTC()
	from := to.Add(-30 * 24 * time.Hour)
	var err error
	if v := strings.TrimSpace(c.Query("to")); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			fail(c, http.StatusBadRequest, "invalid_to", "to must be an RFC 3339 time")
			return store.TurnFilter{}, false
		}
		if strings.TrimSpace(c.Query("from")) == "" {
			from = to.Add(-30 * 24 * time.Hour)
		}
	}
	if v := strings.TrimSpace(c.Query("from")); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			fail(c, http.StatusBadRequest, "invalid_from", "from must be an RFC 3339 time")
			return store.TurnFilter{}, false
		}
	}
	if !to.After(from) {
		fail(c, http.StatusBadRequest, "invalid_range", "to must be later than from")
		return store.TurnFilter{}, false
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && status != traces.StatusOK && status != traces.StatusError {
		fail(c, http.StatusBadRequest, "invalid_status", "status must be ok or error")
		return store.TurnFilter{}, false
	}
	f := store.TurnFilter{
		From: from.UTC(), To: to.UTC(),
		Status: status,
		Query:  strings.TrimSpace(c.Query("q")),
	}
	for _, field := range []struct {
		name string
		dst  *[]string
	}{
		{"project", &f.Project}, {"folder", &f.Folder}, {"client", &f.Client}, {"model", &f.Model},
		{"key", &f.Key}, {"tag", &f.Tag}, {"account", &f.Account}, {"machine", &f.Machine},
	} {
		values, ok := listQuery(c, field.name)
		if !ok {
			return store.TurnFilter{}, false
		}
		*field.dst = values
	}
	return f, true
}

// maxFilterValues bounds the values of one filter field in a request.
const maxFilterValues = 50

// listQuery reads a filter field that may repeat (?tag=a&tag=b). Blank and
// repeated values are dropped.
func listQuery(c *gin.Context, name string) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, v := range c.QueryArray(name) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > maxFilterValues {
		fail(c, http.StatusBadRequest, "too_many_values", fmt.Sprintf("%s takes at most %d values", name, maxFilterValues))
		return nil, false
	}
	return out, true
}

func intQuery(c *gin.Context, name string, def, maxValue int) (int, bool) {
	v := strings.TrimSpace(c.Query(name))
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		fail(c, http.StatusBadRequest, "invalid_"+name, name+" must be a non-negative integer")
		return 0, false
	}
	if maxValue > 0 && n > maxValue {
		n = maxValue
	}
	return n, true
}

func oneOf(c *gin.Context, name, def string, allowed ...string) (string, bool) {
	v := strings.TrimSpace(c.Query(name))
	if v == "" {
		return def, true
	}
	for _, a := range allowed {
		if v == a {
			return v, true
		}
	}
	fail(c, http.StatusBadRequest, "invalid_"+name, name+" must be one of "+strings.Join(allowed, ", "))
	return "", false
}

func (h *Handler) health(c *gin.Context) {
	c.JSON(http.StatusOK, h.svc.Health(c.Request.Context()))
}

func (h *Handler) stats(c *gin.Context) {
	f, ok := h.filter(c)
	if !ok {
		return
	}
	group, ok := oneOf(c, "group", "project", "project", "model", "client", "key", "tag", "account", "machine")
	if !ok {
		return
	}
	bucket, ok := oneOf(c, "bucket", "day", "hour", "day", "week")
	if !ok {
		return
	}
	resp, err := h.svc.Stats(c.Request.Context(), f, group, bucket)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) projects(c *gin.Context) {
	f, ok := h.filter(c)
	if !ok {
		return
	}
	resp, err := h.svc.Projects(c.Request.Context(), f)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) sessions(c *gin.Context) {
	f, ok := h.filter(c)
	if !ok {
		return
	}
	sortBy, ok := oneOf(c, "sort", "started_at", "started_at", "cost", "tokens", "active", "turns")
	if !ok {
		return
	}
	order, ok := oneOf(c, "order", "desc", "asc", "desc")
	if !ok {
		return
	}
	limit, ok := intQuery(c, "limit", 25, 500)
	if !ok {
		return
	}
	offset, ok := intQuery(c, "offset", 0, 0)
	if !ok {
		return
	}
	include := strings.EqualFold(strings.TrimSpace(c.Query("include_subagents")), "true")
	resp, err := h.svc.Sessions(c.Request.Context(), f, traces.SessionQuery{Sort: sortBy, Order: order, Limit: limit, Offset: offset, IncludeSubagents: include})
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) session(c *gin.Context) {
	resp, err := h.svc.SessionDetail(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	if resp == nil {
		fail(c, http.StatusNotFound, "not_found", "no session with this ID")
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) turn(c *gin.Context) {
	resp, err := h.svc.TurnDetail(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	if resp == nil {
		fail(c, http.StatusNotFound, "not_found", "no turn with this ID")
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) block(c *gin.Context) {
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 {
		fail(c, http.StatusBadRequest, "invalid_index", "index must be a non-negative integer")
		return
	}
	block, err := h.svc.TurnBlock(c.Request.Context(), c.Param("id"), index)
	if err != nil {
		internalError(c, err)
		return
	}
	if block == nil {
		fail(c, http.StatusNotFound, "not_found", "no block at this index")
		return
	}
	c.JSON(http.StatusOK, block)
}

func (h *Handler) errors(c *gin.Context) {
	f, ok := h.filter(c)
	if !ok {
		return
	}
	limit, ok := intQuery(c, "limit", 50, 500)
	if !ok {
		return
	}
	offset, ok := intQuery(c, "offset", 0, 0)
	if !ok {
		return
	}
	resp, err := h.svc.Errors(c.Request.Context(), f, limit, offset)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) limits(c *gin.Context) {
	f, ok := h.filter(c)
	if !ok {
		return
	}
	resp, err := h.svc.Limits(c.Request.Context(), f)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func readJSON(c *gin.Context) (ojson.Result, bool) {
	body, err := c.GetRawData()
	if err != nil || !ojson.ValidBytes(body) {
		fail(c, http.StatusBadRequest, "invalid_json", "the body must be a JSON object")
		return ojson.Result{}, false
	}
	root := ojson.ParseBytes(body)
	if !root.IsObject() {
		fail(c, http.StatusBadRequest, "invalid_json", "the body must be a JSON object")
		return ojson.Result{}, false
	}
	return root, true
}

func optionalFloat(r ojson.Result) (*float64, error) {
	switch {
	case !r.Exists() || r.Type == ojson.Null:
		return nil, nil
	case r.Type == ojson.Number:
		v := r.Float()
		if v < 0 {
			return nil, fmt.Errorf("prices must not be negative")
		}
		return &v, nil
	}
	return nil, fmt.Errorf("expected a number or null")
}

func (h *Handler) putLimit(c *gin.Context) {
	root, ok := readJSON(c)
	if !ok {
		return
	}
	value, err := optionalFloat(root.Get("plan_price_usd"))
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_plan_price_usd", err.Error())
		return
	}
	if !h.svc.Enabled() {
		fail(c, http.StatusConflict, "traces_disabled", "turn on traces.enabled to store plan prices")
		return
	}
	account := c.Param("account_id")
	if err = h.svc.SetPlanPrice(c.Request.Context(), account, value); err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"account_id": account, "plan_price_usd": value})
}

func (h *Handler) prices(c *gin.Context) {
	resp, err := h.svc.PricesView(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) saveConfig(c *gin.Context, path []string, value any) bool {
	if h.editor == nil {
		fail(c, http.StatusServiceUnavailable, "config_unavailable", "the config file cannot be edited here")
		return false
	}
	cfg, err := h.editor.SetTracesConfigValue(c.Request.Context(), path, value)
	if err != nil {
		fail(c, http.StatusUnprocessableEntity, "config_save_failed", err.Error())
		return false
	}
	h.svc.Apply(cfg)
	return true
}

func (h *Handler) putPrices(c *gin.Context) {
	root, ok := readJSON(c)
	if !ok {
		return
	}
	list := root.Get("overrides")
	if !list.IsArray() {
		fail(c, http.StatusBadRequest, "invalid_overrides", "overrides must be a list")
		return
	}
	overrides := make([]config.TracesPriceOverride, 0, len(list.Array()))
	for _, item := range list.Array() {
		model := strings.TrimSpace(item.Get("model").String())
		if model == "" {
			fail(c, http.StatusBadRequest, "invalid_overrides", "every override needs a model")
			return
		}
		o := config.TracesPriceOverride{Model: model}
		fields := []struct {
			dst  **float64
			keys []string
		}{
			{&o.Input, []string{"input"}}, {&o.Output, []string{"output"}},
			{&o.CacheRead, []string{"cache_read", "cache-read"}},
			{&o.CacheWrite5m, []string{"cache_write_5m", "cache-write-5m"}},
			{&o.CacheWrite1h, []string{"cache_write_1h", "cache-write-1h"}},
		}
		for _, f := range fields {
			for _, key := range f.keys {
				if v := item.Get(key); v.Exists() {
					value, err := optionalFloat(v)
					if err != nil {
						fail(c, http.StatusBadRequest, "invalid_overrides", model+": "+key+": "+err.Error())
						return
					}
					*f.dst = value
					break
				}
			}
		}
		overrides = append(overrides, o)
	}
	if !h.saveConfig(c, []string{"prices", "overrides"}, overrides) {
		return
	}
	h.prices(c)
}

func (h *Handler) getRules(c *gin.Context) {
	rules := make([]gin.H, 0)
	for _, r := range h.svc.Settings().Rules {
		rules = append(rules, gin.H{"match": r.Match, "project": r.Project})
	}
	c.JSON(http.StatusOK, gin.H{"rules": rules})
}

func parseRules(list ojson.Result) ([]config.TracesProjectRule, error) {
	if !list.IsArray() {
		return nil, fmt.Errorf("rules must be a list")
	}
	rules := make([]config.TracesProjectRule, 0, len(list.Array()))
	for _, item := range list.Array() {
		match := strings.TrimSpace(item.Get("match").String())
		project := strings.TrimSpace(item.Get("project").String())
		if match == "" || project == "" {
			return nil, fmt.Errorf("every rule needs match and project")
		}
		rules = append(rules, config.TracesProjectRule{Match: match, Project: project})
	}
	return rules, nil
}

func (h *Handler) putRules(c *gin.Context) {
	root, ok := readJSON(c)
	if !ok {
		return
	}
	rules, err := parseRules(root.Get("rules"))
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_rules", err.Error())
		return
	}
	if !h.saveConfig(c, []string{"projects", "rules"}, rules) {
		return
	}
	if err = h.svc.ReresolveProjects(c.Request.Context()); err != nil {
		internalError(c, err)
		return
	}
	h.getRules(c)
}

func currentRules(svc *traces.Service) []config.TracesProjectRule {
	var out []config.TracesProjectRule
	for _, r := range svc.Settings().Rules {
		out = append(out, config.TracesProjectRule{Match: r.Match, Project: r.Project})
	}
	return out
}

// updateProject renames a project or merges it into another one by writing
// one exact-folder rule per folder of the project, placed before other rules.
func (h *Handler) updateProject(c *gin.Context) {
	root, ok := readJSON(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	name, folders, err := h.svc.ProjectFolders(ctx, id)
	if err != nil {
		internalError(c, err)
		return
	}
	if name == "" {
		fail(c, http.StatusNotFound, "not_found", "no project with this ID")
		return
	}
	newName := strings.TrimSpace(root.Get("name").String())
	if target := strings.TrimSpace(root.Get("merge_into").String()); target != "" {
		project, errFind := h.svc.FindProject(ctx, target)
		if errFind != nil {
			internalError(c, errFind)
			return
		}
		if project == nil {
			fail(c, http.StatusNotFound, "merge_target_not_found", "no project with the merge_into ID")
			return
		}
		newName = project.Name
	}
	if newName == "" {
		fail(c, http.StatusBadRequest, "invalid_body", "send name or merge_into")
		return
	}
	if len(folders) == 0 {
		fail(c, http.StatusConflict, "no_folders", "this project has no folder to match; set X-CPA-Project in the client instead")
		return
	}
	replace := map[string]bool{}
	for _, f := range folders {
		replace[f] = true
	}
	rules := make([]config.TracesProjectRule, 0, len(folders))
	for _, f := range folders {
		rules = append(rules, config.TracesProjectRule{Match: f, Project: newName})
	}
	for _, r := range currentRules(h.svc) {
		if !replace[r.Match] {
			rules = append(rules, r)
		}
	}
	if !h.saveConfig(c, []string{"projects", "rules"}, rules) {
		return
	}
	if err = h.svc.ReresolveProjects(ctx); err != nil {
		internalError(c, err)
		return
	}
	project, err := h.svc.FindProject(ctx, traces.ProjectID(newName))
	if err != nil {
		internalError(c, err)
		return
	}
	if project == nil {
		project = &traces.Project{ID: traces.ProjectID(newName), Name: newName, Key: newName, Daily: []traces.DailyValue{}}
	}
	c.JSON(http.StatusOK, project)
}

func (h *Handler) export(c *gin.Context) {
	f, ok := h.filter(c)
	if !ok {
		return
	}
	group, ok := oneOf(c, "group", "project", "project", "session")
	if !ok {
		return
	}
	period, ok := oneOf(c, "period", "day", "day", "week", "month")
	if !ok {
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="traces-`+group+`.csv"`)
	c.Status(http.StatusOK)
	if err := h.svc.ExportCSV(c.Request.Context(), f, group, period, c.Writer); err != nil {
		log.WithError(err).Warn("traces: CSV export failed")
	}
}

func (h *Handler) delete(c *gin.Context) {
	if c.Query("confirm") != "true" {
		fail(c, http.StatusBadRequest, "confirm_required", "add confirm=true to delete traces")
		return
	}
	f := store.DeleteFilter{
		SessionID: strings.TrimSpace(c.Query("session")),
		Project:   strings.TrimSpace(c.Query("project")),
		Key:       strings.TrimSpace(c.Query("key")),
	}
	fromRaw, toRaw := strings.TrimSpace(c.Query("from")), strings.TrimSpace(c.Query("to"))
	if (fromRaw == "") != (toRaw == "") {
		fail(c, http.StatusBadRequest, "invalid_range", "send both from and to")
		return
	}
	if fromRaw != "" {
		var err error
		if f.From, err = time.Parse(time.RFC3339, fromRaw); err != nil {
			fail(c, http.StatusBadRequest, "invalid_from", "from must be an RFC 3339 time")
			return
		}
		if f.To, err = time.Parse(time.RFC3339, toRaw); err != nil {
			fail(c, http.StatusBadRequest, "invalid_to", "to must be an RFC 3339 time")
			return
		}
	}
	if f.SessionID == "" && f.Project == "" && f.Key == "" && f.From.IsZero() {
		fail(c, http.StatusBadRequest, "filter_required", "send session, project, key, or from and to")
		return
	}
	resp, err := h.svc.Delete(c.Request.Context(), f)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}
