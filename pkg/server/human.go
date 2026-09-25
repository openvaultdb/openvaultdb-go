package server

import (
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// The default human surface is intentionally neutral. Deployments with their
// own website can serve the same connection URLs themselves and keep the
// machine endpoints and well-known discovery independent of these templates.
//
//go:embed human.css
var humanAssets embed.FS

type humanDB struct {
	ID, Path, Engine, SchemaMode, APIURL, ConnectionURL string
	Collections                                         []string
}

type humanView struct {
	Title, Version string
	Private        bool
	Databases      []humanDB
	Database       humanDB
}

var humanTemplate = template.Must(template.New("human").Parse(`{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Title}} | OpenVaultDB</title><link rel="stylesheet" href="/ovdb/style.css"></head><body><header><nav aria-label="Main"><a class="brand" href="/ovdb/">OpenVaultDB</a><a href="/ovdb/dbs/">Databases</a></nav></header><main>{{end}}
{{define "foot"}}</main><footer>OpenVaultDB {{.Version}}</footer></body></html>{{end}}
{{define "server"}}{{template "head" .}}<p class="eyebrow">OpenVaultDB server</p><h1>OpenVaultDB</h1><p>This server exposes databases through a versioned HTTP API. Browse the public databases or use the discovery document to connect a client.</p>{{if .Private}}<p class="notice">This server requires authentication. Its database catalog is private.</p>{{else}}<p>{{len .Databases}} database{{if ne (len .Databases) 1}}s{{end}} available.</p><a class="button" href="/ovdb/dbs/">Browse databases</a>{{end}}<p><a href="/.well-known/openvaultdb">Discovery document</a> · <a href="/v1/status">Server status API</a></p>{{template "foot" .}}{{end}}
{{define "list"}}{{template "head" .}}<p class="eyebrow">OpenVaultDB server</p><h1>Databases</h1>{{if .Private}}<p class="notice">The database catalog requires authentication. Use an authorized client to query this server.</p>{{else if .Databases}}<ul class="cards">{{range .Databases}}<li><a href="{{.Path}}"><strong>{{.ID}}</strong><span>{{.Engine}} · {{.SchemaMode}} schema</span></a></li>{{end}}</ul>{{else}}<p>No databases are mounted.</p>{{end}}<p><a href="/ovdb/">Back to server</a></p>{{template "foot" .}}{{end}}
{{define "database"}}{{template "head" .}}<p class="eyebrow"><a href="/ovdb/dbs/">Databases</a> / profile</p><h1>{{.Database.ID}}</h1><p>This URL identifies the database. Machine operations use separately versioned endpoints.</p><dl><dt>Connection URL</dt><dd><code>{{.Database.ConnectionURL}}</code></dd><dt>Storage engine</dt><dd>{{.Database.Engine}}</dd><dt>Schema mode</dt><dd>{{.Database.SchemaMode}}</dd><dt>Declared collections</dt><dd>{{if .Database.Collections}}{{range $i, $name := .Database.Collections}}{{if $i}}, {{end}}<code>{{$name}}</code>{{end}}{{else}}No declared collection list{{end}}</dd></dl><p><a href="{{.Database.APIURL}}">Machine metadata</a> · <a href="/.well-known/openvaultdb">Discovery document</a></p>{{template "foot" .}}{{end}}
{{define "missing"}}{{template "head" .}}<h1>Database not found</h1><p>No public database exists at this URL.</p><p><a href="/ovdb/dbs/">Browse databases</a></p>{{template "foot" .}}{{end}}`))

func humanDatabasePath(id string) string { return "/ovdb/dbs/" + url.PathEscape(id) }

func (s *Server) humanOrigin(r *http.Request) string {
	if s.publicOrigin != "" {
		return s.publicOrigin
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) humanDatabases(r *http.Request) []humanDB {
	if s.authCfg != nil {
		return nil
	}
	origin := s.humanOrigin(r)
	ids := s.databaseIDs()
	result := make([]humanDB, 0, len(ids))
	for _, id := range ids {
		db := s.getDB(id)
		if db == nil {
			continue
		}
		collections := make([]string, 0)
		if db.Manifest.Schemas != nil {
			for name := range db.Manifest.Schemas.Collections {
				collections = append(collections, name)
			}
			sort.Strings(collections)
		}
		result = append(result, humanDB{ID: id, Path: humanDatabasePath(id), Engine: db.Manifest.Storage.Engine, SchemaMode: string(db.Manifest.Database.SchemaMode), APIURL: origin + "/v1/databases/" + url.PathEscape(id), ConnectionURL: origin + humanDatabasePath(id), Collections: collections})
	}
	return result
}

func (s *Server) writeHuman(w http.ResponseWriter, r *http.Request, status int, page string, view humanView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Link", "</.well-known/openvaultdb>; rel=\"describedby\"; type=\"application/json\"")
	w.WriteHeader(status)
	_ = humanTemplate.ExecuteTemplate(w, page, view)
}

func (s *Server) handleHumanServer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ovdb/" {
		http.NotFound(w, r)
		return
	}
	s.writeHuman(w, r, http.StatusOK, "server", humanView{Title: "Server", Version: s.version, Private: s.authCfg != nil, Databases: s.humanDatabases(r)})
}

func (s *Server) handleHumanDatabases(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ovdb/dbs/" {
		http.NotFound(w, r)
		return
	}
	s.writeHuman(w, r, http.StatusOK, "list", humanView{Title: "Databases", Version: s.version, Private: s.authCfg != nil, Databases: s.humanDatabases(r)})
}

func (s *Server) handleHumanDatabase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("db")
	view := humanView{Title: "Database not found", Version: s.version}
	if s.authCfg == nil && id != "" && !strings.Contains(id, "/") {
		for _, db := range s.humanDatabases(r) {
			if db.ID == id {
				view.Title, view.Database = db.ID, db
				s.writeHuman(w, r, http.StatusOK, "database", view)
				return
			}
		}
	}
	s.writeHuman(w, r, http.StatusNotFound, "missing", view)
}

func handleHumanStyle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	data, _ := humanAssets.ReadFile("human.css")
	_, _ = w.Write(data)
}
