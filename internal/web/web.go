package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
)

// logText HTML-escapes log content and then encodes carriage returns as
// &#13;. The HTML tokenizer normalizes raw \r to \n BEFORE entity decoding,
// so this is the only way the client's CR-collapsing renderer can see the
// real \r bytes in server-rendered log text.
var tmplFuncs = template.FuncMap{
	"logText": func(s string) template.HTML {
		return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(s), "\r", "&#13;"))
	},
}

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Handler struct {
	DB       *db.DB
	BasePath string
}

func New(database *db.DB, basePath string) *Handler {
	return &Handler{DB: database, BasePath: basePath}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Pages
	mux.HandleFunc("GET /", h.handleIndex)
	mux.HandleFunc("GET /projects/{id}", h.handleProject)
	mux.HandleFunc("GET /builds/{id}", h.handleBuild)
}

func (h *Handler) render(w http.ResponseWriter, name string, data map[string]interface{}) {
	tmpl, err := template.New(name).Funcs(tmplFuncs).ParseFS(templateFS, "templates/base.html", "templates/"+name+".html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	data["BasePath"] = h.BasePath
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(w, "base", data)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	projects, err := h.DB.ListProjects()
	if err != nil {
		http.Error(w, "failed to load projects: "+err.Error(), 500)
		return
	}
	builds, err := h.DB.ListRecentBuilds(10)
	if err != nil {
		http.Error(w, "failed to load builds: "+err.Error(), 500)
		return
	}
	h.render(w, "index", map[string]interface{}{
		"Title":    "Builds",
		"Projects": projects,
		"Builds":   builds,
	})
}

func (h *Handler) handleProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	project, err := h.DB.GetProject(id)
	if err != nil {
		http.Error(w, "project not found", 404)
		return
	}
	builds, err := h.DB.ListBuildsByProject(id, 0)
	if err != nil {
		http.Error(w, "failed to load builds: "+err.Error(), 500)
		return
	}
	h.render(w, "project", map[string]interface{}{
		"Title":   project.Name,
		"Project": project,
		"Builds":  builds,
	})
}

func (h *Handler) handleBuild(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	build, err := h.DB.GetBuild(id)
	if err != nil {
		http.Error(w, "build not found", 404)
		return
	}
	// Project may be gone if deleted after the build; the page degrades.
	project, _ := h.DB.GetProject(build.ProjectID)
	queuePos := 0
	if build.Status == models.StatusPending {
		queuePos, _ = h.DB.QueuePosition(build.ID)
	}
	h.render(w, "build", map[string]interface{}{
		"Title":    build.ProjectName + " #" + strconv.FormatInt(build.ID, 10),
		"Build":    build,
		"Project":  project,
		"QueuePos": queuePos,
	})
}
