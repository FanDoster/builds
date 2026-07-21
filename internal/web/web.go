package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/FanDoster/builds/internal/db"
)

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
	tmpl, err := template.ParseFS(templateFS, "templates/base.html", "templates/"+name+".html")
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
	projects, _ := h.DB.ListProjects()
	builds, _ := h.DB.ListRecentBuilds(10)
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
	builds, _ := h.DB.ListBuildsByProject(id, 0)
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
	h.render(w, "build", map[string]interface{}{
		"Title": build.ProjectName + " #" + strconv.FormatInt(build.ID, 10),
		"Build": build,
	})
}
