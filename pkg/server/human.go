package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/dal-go/dalgo/dbschema"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// The default human surface is intentionally neutral. Deployments with their
// own website can serve the same connection URLs themselves and keep the
// machine endpoints and well-known discovery independent of these templates.
//
//go:embed human.css
var humanAssets embed.FS

type humanDB struct {
	ID, Path, Engine, SchemaMode, APIURL, ConnectionURL string
	Collections                                         []humanCollection
	Protected                                           bool
}

type humanField struct {
	Name, Type string
	Required   bool
}

type humanCollection struct {
	Name, Path, QueryURL string
	Fields               []humanField
	References           []humanReference
	ReferencedBy         []humanReference
}

type humanReference struct {
	Field, Collection, TargetField, Path string
	Source, Enforcement, Name            string
	OnUpdate, OnDelete                   string
	fields, targetFields                 []string
}

type humanView struct {
	Title, Version string
	Private        bool
	Databases      []humanDB
	Database       humanDB
	Collection     humanCollection
}

var humanTemplate = template.Must(template.New("human").Parse(`{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Title}} | OpenVaultDB</title><link rel="stylesheet" href="/ovdb/style.css"></head><body><header><nav aria-label="Main"><a class="brand" href="/ovdb/">OpenVaultDB</a><a href="/ovdb/dbs/">Databases</a></nav></header><main>{{end}}
{{define "foot"}}</main><footer>OpenVaultDB {{.Version}}</footer></body></html>{{end}}
{{define "server"}}{{template "head" .}}<p class="eyebrow">OpenVaultDB server</p><h1>OpenVaultDB</h1><p>This server exposes databases through a versioned HTTP API. Browse the public databases or use the discovery document to connect a client.</p>{{if .Private}}<p class="notice">This server requires authentication. Its database catalog is private.</p>{{else}}<p>{{len .Databases}} database{{if ne (len .Databases) 1}}s{{end}} available.</p><a class="button" href="/ovdb/dbs/">Browse databases</a>{{end}}<p><a href="/.well-known/openvaultdb">Discovery document</a> · <a href="/v1/status">Server status API</a></p>{{template "foot" .}}{{end}}
{{define "list"}}{{template "head" .}}<p class="eyebrow">OpenVaultDB server</p><h1>Databases</h1>{{if .Private}}<p class="notice">The database catalog requires authentication. Use an authorized client to query this server.</p>{{else if .Databases}}<ul class="cards">{{range .Databases}}<li><a href="{{.Path}}"><strong>{{.ID}}</strong><span>{{.Engine}} · {{.SchemaMode}} schema</span></a></li>{{end}}</ul>{{else}}<p>No databases are mounted.</p>{{end}}<p><a href="/ovdb/">Back to server</a></p>{{template "foot" .}}{{end}}
{{define "database"}}{{template "head" .}}<p class="eyebrow"><a href="/ovdb/dbs/">Databases</a> / profile</p><h1>{{.Database.ID}}</h1><p>This URL identifies the database. Machine operations use separately versioned endpoints.</p><dl><dt>Connection URL</dt><dd><code>{{.Database.ConnectionURL}}</code></dd><dt>Storage engine</dt><dd>{{.Database.Engine}}</dd><dt>Schema mode</dt><dd>{{.Database.SchemaMode}}</dd></dl><h2>Declared collections</h2>{{if .Database.Protected}}<p>This database's collection schema requires an authorized client.</p>{{else if .Database.Collections}}<ul class="cards">{{range .Database.Collections}}<li><a href="{{.Path}}"><strong>{{.Name}}</strong><span>{{len .Fields}} declared fields</span></a></li>{{end}}</ul>{{else}}<p>No declared collections.</p>{{end}}<p><a href="{{.Database.APIURL}}">Machine metadata</a> · <a href="/.well-known/openvaultdb">Discovery document</a></p>{{template "foot" .}}{{end}}
{{define "collection"}}{{template "head" .}}<p class="eyebrow"><a href="/ovdb/dbs/">Databases</a> / <a href="{{.Database.Path}}">{{.Database.ID}}</a> / collection</p><h1>{{.Collection.Name}}</h1><p>Declared collection schema in the {{.Database.ID}} database.</p>{{if .Collection.Fields}}<div class="table-scroll"><table><thead><tr><th scope="col">Field</th><th scope="col">Type</th><th scope="col">Required</th></tr></thead><tbody>{{range .Collection.Fields}}<tr><th scope="row"><code>{{.Name}}</code></th><td>{{.Type}}</td><td>{{if .Required}}Yes{{else}}No{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p>No fields are declared for this collection.</p>{{end}}<h2>References</h2>{{if .Collection.References}}<ul>{{range .Collection.References}}<li><code>{{.Field}}</code> → {{if .Path}}<a href="{{.Path}}">{{.Collection}}</a>{{else}}{{.Collection}}{{end}}{{if .TargetField}}.<code>{{.TargetField}}</code>{{end}} <small>({{.Source}}; enforcement: {{.Enforcement}}{{if .Name}}; key: {{.Name}}{{end}}{{if .OnDelete}}; on delete: {{.OnDelete}}{{end}}{{if .OnUpdate}}; on update: {{.OnUpdate}}{{end}})</small></li>{{end}}</ul>{{else}}<p>No outgoing references.</p>{{end}}<h2>Referenced by</h2>{{if .Collection.ReferencedBy}}<ul>{{range .Collection.ReferencedBy}}<li><a href="{{.Path}}">{{.Collection}}</a>.<code>{{.Field}}</code>{{if .TargetField}} → <code>{{.TargetField}}</code>{{end}} <small>({{.Source}}; enforcement: {{.Enforcement}}{{if .Name}}; key: {{.Name}}{{end}}{{if .OnDelete}}; on delete: {{.OnDelete}}{{end}}{{if .OnUpdate}}; on update: {{.OnUpdate}}{{end}})</small></li>{{end}}</ul>{{else}}<p>No incoming references.</p>{{end}}<p><a href="{{.Database.APIURL}}">Database metadata API</a> · <a href="{{.Collection.QueryURL}}">Query records (JSON)</a></p>{{template "foot" .}}{{end}}
{{define "missing"}}{{template "head" .}}<h1>Database not found</h1><p>No public database exists at this URL.</p><p><a href="/ovdb/dbs/">Browse databases</a></p>{{template "foot" .}}{{end}}
{{define "missingCollection"}}{{template "head" .}}<h1>Collection not found</h1><p>No declared public collection exists at this URL.</p><p><a href="/ovdb/dbs/">Browse databases</a></p>{{template "foot" .}}{{end}}`))

func humanDatabasePath(id string) string { return "/ovdb/dbs/" + url.PathEscape(id) }

func humanCollectionPath(databaseID, collection string) string {
	return humanDatabasePath(databaseID) + "/collections/" + url.PathEscape(collection)
}

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

func (s *Server) humanDatabases(r *http.Request, includeProviderReferences bool, requestedID string) ([]humanDB, error) {
	if s.authCfg != nil {
		return nil, nil
	}
	origin := s.humanOrigin(r)
	ids := s.databaseIDs()
	result := make([]humanDB, 0, len(ids))
	for _, id := range ids {
		if requestedID != "" && id != requestedID {
			continue
		}
		db := s.getDB(id)
		if db == nil {
			continue
		}
		collections := make([]humanCollection, 0)
		protected := db.HasAccessPolicies()
		if db.Manifest.Schemas != nil && !protected {
			for name, schema := range db.Manifest.Schemas.Collections {
				fields := make([]humanField, 0, len(schema.Fields))
				for fieldName, field := range schema.Fields {
					fields = append(fields, humanField{Name: fieldName, Type: string(field.Type), Required: field.Required})
				}
				sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
				queryJSON, err := json.Marshal(core.Query{Collection: name, Limit: 50})
				if err != nil {
					return nil, fmt.Errorf("collection %q query link: %w", name, err)
				}
				query := url.Values{"q": {string(queryJSON)}}
				references := make([]humanReference, 0, len(schema.References))
				for _, ref := range schema.References {
					fields, targets := []string{ref.Field}, []string{ref.TargetField}
					if len(ref.Fields) > 0 {
						fields, targets = ref.Fields, ref.TargetFields
					}
					references = append(references, humanReference{Field: strings.Join(fields, ", "), Collection: ref.Collection, TargetField: strings.Join(targets, ", "), Path: humanCollectionPath(id, ref.Collection), Source: "OVDB declaration", Enforcement: "Informational", fields: fields, targetFields: targets})
				}
				var foreignKeys []dbschema.ForeignKeyDef
				if includeProviderReferences {
					var err error
					foreignKeys, err = db.CollectionForeignKeys(r.Context(), name)
					if err != nil {
						return nil, fmt.Errorf("database %q: %w", id, err)
					}
				}
				for _, fk := range foreignKeys {
					target := fk.ReferencedCollection
					if fk.ReferencedNamespace != "" {
						target = fk.ReferencedNamespace + "." + target
					}
					ref := humanReference{Collection: target, Source: "Database", Enforcement: string(fk.Enforcement), Name: fk.Name, OnUpdate: fk.OnUpdate, OnDelete: fk.OnDelete}
					for i, field := range fk.Fields {
						if i > 0 {
							ref.Field += ", "
						}
						ref.Field += string(field)
						ref.fields = append(ref.fields, string(field))
					}
					for i, field := range fk.ReferencedFields {
						if i > 0 {
							ref.TargetField += ", "
						}
						ref.TargetField += string(field)
						ref.targetFields = append(ref.targetFields, string(field))
					}
					if _, ok := db.Manifest.Schemas.Collections[fk.ReferencedCollection]; ok && fk.ReferencedNamespace == "" {
						ref.Path = humanCollectionPath(id, target)
					}
					merged := false
					for i := range references {
						if fk.ReferencedNamespace == "" && references[i].Source == "OVDB declaration" && references[i].Collection == fk.ReferencedCollection && slices.Equal(references[i].fields, ref.fields) && slices.Equal(references[i].targetFields, ref.targetFields) {
							references[i].Source = "Database and OVDB declaration"
							references[i].Enforcement = ref.Enforcement
							references[i].Name = ref.Name
							references[i].OnUpdate = ref.OnUpdate
							references[i].OnDelete = ref.OnDelete
							merged = true
							break
						}
					}
					if !merged {
						references = append(references, ref)
					}
				}
				sort.Slice(references, func(i, j int) bool {
					if references[i].Collection != references[j].Collection {
						return references[i].Collection < references[j].Collection
					}
					return references[i].Field < references[j].Field
				})
				collections = append(collections, humanCollection{Name: name, Path: humanCollectionPath(id, name), QueryURL: origin + "/v1/databases/" + url.PathEscape(id) + "/query?" + query.Encode(), Fields: fields, References: references})
			}
			sort.Slice(collections, func(i, j int) bool { return collections[i].Name < collections[j].Name })
			for source := range collections {
				for _, ref := range collections[source].References {
					for target := range collections {
						if ref.Path != "" && collections[target].Name == ref.Collection {
							collections[target].ReferencedBy = append(collections[target].ReferencedBy, humanReference{Field: ref.Field, Collection: collections[source].Name, TargetField: ref.TargetField, Path: collections[source].Path, Source: ref.Source, Enforcement: ref.Enforcement, Name: ref.Name, OnUpdate: ref.OnUpdate, OnDelete: ref.OnDelete})
							break
						}
					}
				}
			}
		}
		result = append(result, humanDB{ID: id, Path: humanDatabasePath(id), Engine: db.Manifest.Storage.Engine, SchemaMode: string(db.Manifest.Database.SchemaMode), APIURL: origin + "/v1/databases/" + url.PathEscape(id), ConnectionURL: origin + humanDatabasePath(id), Collections: collections, Protected: protected})
	}
	return result, nil
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

func (s *Server) humanDatabasesOrError(w http.ResponseWriter, r *http.Request, includeProviderReferences bool, requestedID string) ([]humanDB, bool) {
	databases, err := s.humanDatabases(r, includeProviderReferences, requestedID)
	if err != nil {
		http.Error(w, "Database metadata unavailable", http.StatusInternalServerError)
		return nil, false
	}
	return databases, true
}

func (s *Server) handleHumanServer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ovdb/" {
		http.NotFound(w, r)
		return
	}
	databases, ok := s.humanDatabasesOrError(w, r, false, "")
	if !ok {
		return
	}
	s.writeHuman(w, r, http.StatusOK, "server", humanView{Title: "Server", Version: s.version, Private: s.authCfg != nil, Databases: databases})
}

func (s *Server) handleHumanDatabases(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ovdb/dbs/" {
		http.NotFound(w, r)
		return
	}
	databases, ok := s.humanDatabasesOrError(w, r, false, "")
	if !ok {
		return
	}
	s.writeHuman(w, r, http.StatusOK, "list", humanView{Title: "Databases", Version: s.version, Private: s.authCfg != nil, Databases: databases})
}

func (s *Server) handleHumanDatabase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("db")
	view := humanView{Title: "Database not found", Version: s.version}
	if s.authCfg == nil && id != "" && !strings.Contains(id, "/") {
		databases, ok := s.humanDatabasesOrError(w, r, false, id)
		if !ok {
			return
		}
		for _, db := range databases {
			if db.ID == id {
				view.Title, view.Database = db.ID, db
				s.writeHuman(w, r, http.StatusOK, "database", view)
				return
			}
		}
	}
	s.writeHuman(w, r, http.StatusNotFound, "missing", view)
}

func (s *Server) handleHumanCollection(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("db"), r.PathValue("collection")
	view := humanView{Title: "Collection not found", Version: s.version}
	if s.authCfg == nil && id != "" && name != "" && !strings.Contains(id, "/") {
		mounted := s.getDB(id)
		if mounted == nil || mounted.HasAccessPolicies() || mounted.Manifest.Schemas.Collection(name) == nil {
			s.writeHuman(w, r, http.StatusNotFound, "missingCollection", view)
			return
		}
		databases, ok := s.humanDatabasesOrError(w, r, true, id)
		if !ok {
			return
		}
		for _, db := range databases {
			if db.ID != id {
				continue
			}
			for _, collection := range db.Collections {
				if collection.Name == name {
					view.Title, view.Database, view.Collection = collection.Name, db, collection
					s.writeHuman(w, r, http.StatusOK, "collection", view)
					return
				}
			}
		}
	}
	s.writeHuman(w, r, http.StatusNotFound, "missingCollection", view)
}

func handleHumanStyle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	data, _ := humanAssets.ReadFile("human.css")
	_, _ = w.Write(data)
}
