package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/dal-go/dalgo/dbschema"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"gopkg.in/yaml.v3"
)

// The default human surface is intentionally neutral. Deployments with their
// own website can serve the same connection URLs themselves and keep the
// machine endpoints and well-known discovery independent of these templates.
//
//go:embed human.css
var humanAssets embed.FS

type humanDB struct {
	Rights                                              []license.SourceRight
	ID, Path, Engine, SchemaMode, APIURL, ConnectionURL string
	Tags                                                []string
	Collections                                         []humanCollection
	Protected, NativeReadOnly                           bool
	NoRetention                                         bool
}

type humanField struct {
	Name, Type, NativeType         string
	Required, Nullable, PrimaryKey bool
}

type humanCollection struct {
	Rights                   []license.SourceRight
	ID, Name, Path, QueryURL string
	Fields                   []humanField
	References               []humanReference
	ReferencedBy             []humanReference
}

type humanReference struct {
	Field, Collection, TargetField, Path string
	Source, Enforcement, Name            string
	OnUpdate, OnDelete                   string
	fields, targetFields                 []string
}

type humanView struct {
	Rights         []license.SourceRight
	Title, Version string
	Private        bool
	Databases      []humanDB
	Database       humanDB
	Collection     humanCollection
}

var humanTemplate = template.Must(template.New("human").Parse(`{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Title}} | OpenVaultDB</title><link rel="stylesheet" href="/ovdb/style.css"></head><body><header><nav aria-label="Main"><a class="brand" href="/ovdb/">OpenVaultDB</a><a href="/ovdb/dbs/">Databases</a></nav></header><main>{{end}}
{{define "foot"}}</main><footer>OpenVaultDB {{.Version}}</footer></body></html>{{end}}
{{define "rights"}}<section aria-label="Source data terms"><h2>Source data terms</h2>{{if .}}<p>These source data terms do not grant an output license.</p>{{range .}}<p>Declared at {{.DeclarationScope}} scope{{if .DeclaredAt.DatabaseID}} · {{.DeclaredAt.DatabaseID}}{{end}}{{if .DeclaredAt.Recordset}} / {{.DeclaredAt.Recordset}}{{end}}.</p>{{with .Declaration}}<p>{{if .Name}}<strong>{{.Name}}</strong>{{end}}{{if .SPDX}} <code>{{.SPDX}}</code>{{end}}</p>{{if .URL}}<p><a href="{{.URL}}" rel="external noreferrer">Source terms</a></p>{{end}}{{if .Text}}<pre class="source-terms">{{.Text}}</pre>{{end}}{{end}}{{with .Attribution}}<p><strong>Attribution:</strong> {{.Text}}{{if .URL}} · <a href="{{.URL}}" rel="external noreferrer">Attribution source</a>{{end}}</p>{{end}}{{with .FreeSource}}<p><strong>Original free source:</strong> {{.Text}}{{if .URL}} · <a href="{{.URL}}" rel="external noreferrer">{{.URL}}</a>{{end}}</p>{{end}}{{if .Transformations}}<h3>Transformations</h3><ul>{{range .Transformations}}<li>{{.}}</li>{{end}}</ul>{{end}}{{end}}{{else}}<p>Source data terms not declared.</p>{{end}}</section>{{end}}
{{define "server"}}{{template "head" .}}<p class="eyebrow">OpenVaultDB server</p><h1>OpenVaultDB</h1>{{template "rights" .Rights}}<p>This server exposes databases through a versioned HTTP API. Browse the public databases or use the discovery document to connect a client.</p>{{if .Private}}<p class="notice">This server requires authentication. Its database catalog is private.</p>{{else}}<p>{{len .Databases}} database{{if ne (len .Databases) 1}}s{{end}} available.</p><a class="button" href="/ovdb/dbs/">Browse databases</a>{{end}}<p><a href="/.well-known/openvaultdb">Discovery document</a> · <a href="/v1/status">Server status API</a></p>{{template "foot" .}}{{end}}
{{define "list"}}{{template "head" .}}<p class="eyebrow">OpenVaultDB server</p><h1>Databases</h1>{{if .Private}}<p class="notice">The database catalog requires authentication. Use an authorized client to query this server.</p>{{else if .Databases}}<ul class="cards">{{range .Databases}}<li><a href="{{.Path}}"><strong>{{.ID}}</strong><span>{{.Engine}} · {{.SchemaMode}} schema{{if .Tags}} · Tags: {{range $i, $tag := .Tags}}{{if $i}}, {{end}}{{$tag}}{{end}}{{end}}</span></a></li>{{end}}</ul>{{else}}<p>No databases are mounted.</p>{{end}}<p><a href="/ovdb/">Back to server</a></p>{{template "foot" .}}{{end}}
{{define "database"}}{{template "head" .}}<p class="eyebrow"><a href="/ovdb/dbs/">Databases</a> / profile</p><h1>{{.Database.ID}}</h1>{{template "rights" .Database.Rights}}{{if .Database.NoRetention}}<p class="notice">Retention: none. Source responses and query results are not retained by this server.</p>{{end}}<p>This URL identifies the database. Machine operations use separately versioned endpoints.</p><dl><dt>Connection URL</dt><dd><code>{{.Database.ConnectionURL}}</code></dd><dt>Storage engine</dt><dd>{{.Database.Engine}}</dd><dt>Schema mode</dt><dd>{{.Database.SchemaMode}}</dd>{{if .Database.NativeReadOnly}}<dt>Access</dt><dd>Native PostgreSQL, read-only structured queries</dd>{{end}}{{if .Database.Tags}}<dt>Tags</dt><dd>{{range $i, $tag := .Database.Tags}}{{if $i}}, {{end}}{{$tag}}{{end}}</dd>{{end}}</dl><h2>{{if .Database.NativeReadOnly}}Available collections{{else}}Declared collections{{end}}</h2>{{if .Database.Protected}}<p>This database's collection schema requires an authorized client.</p>{{else if .Database.Collections}}<ul class="cards">{{range .Database.Collections}}<li><a href="{{.Path}}"><strong>{{.Name}}</strong><span>{{len .Fields}} fields</span></a></li>{{end}}</ul>{{else}}<p>No declared collections.</p>{{end}}<p><a href="{{.Database.APIURL}}">Machine metadata</a> · <a href="/.well-known/openvaultdb">Discovery document</a></p>{{template "foot" .}}{{end}}
{{define "collection"}}{{template "head" .}}<p class="eyebrow"><a href="/ovdb/dbs/">Databases</a> / <a href="{{.Database.Path}}">{{.Database.ID}}</a> / collection</p><h1>{{.Collection.Name}}</h1>{{template "rights" .Collection.Rights}}{{if .Database.NoRetention}}<p class="notice">Retention: none. Source responses and query results are not retained by this server.</p>{{end}}<p>{{if .Database.NativeReadOnly}}Native PostgreSQL relation in the {{.Database.ID}} database; reads use bounded structured queries.{{else}}Declared collection schema in the {{.Database.ID}} database.{{end}}</p>{{if .Collection.Fields}}<div class="table-scroll"><table><thead><tr><th scope="col">Field</th><th scope="col">Type</th>{{if .Database.NativeReadOnly}}<th scope="col">Native type</th>{{end}}<th scope="col">Required</th>{{if .Database.NativeReadOnly}}<th scope="col">Primary key</th>{{end}}</tr></thead><tbody>{{range .Collection.Fields}}<tr><th scope="row"><code>{{.Name}}</code></th><td>{{.Type}}</td>{{if $.Database.NativeReadOnly}}<td>{{.NativeType}}</td>{{end}}<td>{{if .Required}}Yes{{else}}No{{end}}</td>{{if $.Database.NativeReadOnly}}<td>{{if .PrimaryKey}}Yes{{else}}No{{end}}</td>{{end}}</tr>{{end}}</tbody></table></div>{{else}}<p>No fields are declared for this collection.</p>{{end}}<h2>References</h2>{{if .Collection.References}}<ul>{{range .Collection.References}}<li><code>{{.Field}}</code> → {{if .Path}}<a href="{{.Path}}">{{.Collection}}</a>{{else}}{{.Collection}}{{end}}{{if .TargetField}}.<code>{{.TargetField}}</code>{{end}} <small>({{.Source}}; enforcement: {{.Enforcement}}{{if .Name}}; key: {{.Name}}{{end}}{{if .OnDelete}}; on delete: {{.OnDelete}}{{end}}{{if .OnUpdate}}; on update: {{.OnUpdate}}{{end}})</small></li>{{end}}</ul>{{else}}<p>No outgoing references.</p>{{end}}<h2>Referenced by</h2>{{if .Collection.ReferencedBy}}<ul>{{range .Collection.ReferencedBy}}<li><a href="{{.Path}}">{{.Collection}}</a>.<code>{{.Field}}</code>{{if .TargetField}} → <code>{{.TargetField}}</code>{{end}} <small>({{.Source}}; enforcement: {{.Enforcement}}{{if .Name}}; key: {{.Name}}{{end}}{{if .OnDelete}}; on delete: {{.OnDelete}}{{end}}{{if .OnUpdate}}; on update: {{.OnUpdate}}{{end}})</small></li>{{end}}</ul>{{else}}<p>No incoming references.</p>{{end}}<p><a href="{{.Database.APIURL}}">Database metadata API</a>{{if .Collection.QueryURL}} · <a href="{{.Collection.QueryURL}}">{{if .Database.NativeReadOnly}}Run bounded sample query{{else}}Query records (JSON){{end}}</a>{{end}}</p>{{template "foot" .}}{{end}}
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
					fields = append(fields, humanField{Name: fieldName, Type: string(field.Type), NativeType: field.NativeType, Required: field.Required, Nullable: field.Nullable, PrimaryKey: field.PrimaryKey})
				}
				sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
				queryName := name
				if canonical, declared := db.CanonicalCollection(name); declared {
					queryName = canonical
				}
				query := url.Values{}
				displayName := name
				if source, native := db.NativePostgresSource(name); native {
					displayName = source.Schema + "." + source.Name
					doc, err := yaml.Marshal(map[string]any{"from": map[string]string{"schema": source.Schema, "name": source.Name}, "limit": 50})
					if err != nil {
						return nil, fmt.Errorf("native collection query link: %w", err)
					}
					query.Set("q", string(doc))
				} else {
					queryJSON, err := json.Marshal(core.Query{Collection: queryName, Limit: 50})
					if err != nil {
						return nil, fmt.Errorf("collection %q query link: %w", name, err)
					}
					query.Set("q", string(queryJSON))
				}
				references := make([]humanReference, 0, len(schema.References))
				for _, ref := range schema.References {
					if s.boundedImmutable(db) {
						if _, declared := db.CanonicalCollection(ref.Collection); !declared {
							continue
						}
					}
					fields, targets := []string{ref.Field}, []string{ref.TargetField}
					if len(ref.Fields) > 0 {
						fields, targets = ref.Fields, ref.TargetFields
					}
					references = append(references, humanReference{Field: strings.Join(fields, ", "), Collection: ref.Collection, TargetField: strings.Join(targets, ", "), Path: humanCollectionPath(id, ref.Collection), Source: "OVDB declaration", Enforcement: "Informational", fields: fields, targetFields: targets})
				}
				var foreignKeys []dbschema.ForeignKeyDef
				if includeProviderReferences {
					var err error
					foreignKeys, err = db.CollectionForeignKeys(r.Context(), queryName)
					if err != nil {
						return nil, fmt.Errorf("database %q: %w", id, err)
					}
				}
				for _, fk := range foreignKeys {
					target := fk.ReferencedCollection
					targetID := fk.ReferencedCollection
					if db.NativePostgresReadOnly() {
						if logical, ok := db.ResolveNativePostgresCollection(fk.ReferencedNamespace, fk.ReferencedCollection); ok {
							targetID = logical
						}
						target = fk.ReferencedNamespace + "." + target
					} else if fk.ReferencedNamespace != "" {
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
					if _, ok := db.Manifest.Schemas.Collections[targetID]; ok {
						ref.Path = humanCollectionPath(id, targetID)
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
				queryURL := ""
				if db.CanQuery() && !s.boundedImmutable(db) {
					endpoint := "/query"
					if db.NativePostgresReadOnly() {
						endpoint = "/dtql"
					}
					queryURL = origin + "/v1/databases/" + url.PathEscape(id) + endpoint + "?" + query.Encode()
				}
				rights, err := s.databaseRights(db, queryName)
				if err != nil {
					return nil, err
				}
				collections = append(collections, humanCollection{Rights: rights, ID: name, Name: displayName, Path: humanCollectionPath(id, name), QueryURL: queryURL, Fields: fields, References: references})
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
		// The database page describes the active dynamic source when one is admitted.
		// Other databases keep their existing declaration-level terms.
		rightsCollection := ""
		if profile, ok := s.providerProfilesByDB[db]; ok && !protected {
			rightsCollection = profile.Collection
		}
		rights, err := s.databaseRights(db, rightsCollection)
		if err != nil {
			return nil, err
		}
		result = append(result, humanDB{Rights: rights, ID: id, Path: humanDatabasePath(id), Engine: db.Manifest.Storage.Engine, SchemaMode: string(db.Manifest.Database.SchemaMode), Tags: db.Manifest.Database.Tags, APIURL: origin + "/v1/databases/" + url.PathEscape(id), ConnectionURL: origin + humanDatabasePath(id), Collections: collections, Protected: protected, NativeReadOnly: db.NativePostgresReadOnly(), NoRetention: db.Manifest.EffectiveRetention() == manifest.RetentionNone && db.NoRetention()})
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
	for _, id := range s.databaseIDs() {
		if requestedID != "" && id != requestedID {
			continue
		}
		if db := s.getDB(id); db != nil && !s.guardRetentionRead(w, r, db) {
			return nil, false
		}
	}
	databases, err := s.humanDatabases(r, includeProviderReferences, requestedID)
	if err != nil {
		if errors.Is(err, core.ErrDatabaseUnreachable) {
			s.logUnreachable(r, err)
			http.Error(w, "Database metadata unavailable", http.StatusServiceUnavailable)
			return nil, false
		}
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
	s.writeHuman(w, r, http.StatusOK, "server", humanView{Rights: s.serverRights(), Title: "Server", Version: s.version, Private: s.authCfg != nil, Databases: databases})
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
				if collection.ID == name {
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
