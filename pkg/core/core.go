// Package core is the thin OpenVaultDB layer between the HTTP API and DALgo
// drivers. Storage access goes through dal.DB natively — inGitDB via
// dalgo2ingitdb, SQLite via dalgo2sqlite — so ovdb's job is schema-mode
// enforcement, collection provisioning (via ddl.SchemaModifier), inferred
// schema observation, and (future) authentication. Reads, writes, updates and
// queries pass through to the driver.
package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/inferred"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// ErrUpdateOfMissingRecord is returned when an update op targets a record
// that neither exists in the store nor was written earlier in the batch.
var ErrUpdateOfMissingRecord = errors.New("cannot update: record not found")

// ModeCompatibilityError is the loud failure for a schema mode a driver
// does not support.
type ModeCompatibilityError struct {
	Engine    string
	Requested schema.Mode
	Supported []schema.Mode
}

func (e *ModeCompatibilityError) Error() string {
	supported := make([]string, len(e.Supported))
	for i, m := range e.Supported {
		supported[i] = string(m)
	}
	return fmt.Sprintf("requested schema mode %q is not supported by %s engine in MVP; supported schema modes for %s: %s",
		e.Requested, e.Engine, e.Engine, strings.Join(supported, ", "))
}

// Database is one mounted logical database: a DALgo driver plus mode
// enforcement.
type Database struct {
	Manifest *manifest.Manifest
	db       dal.DB
	modes    []schema.Mode
	cat      *inferred.Catalogue // nil in strict mode

	// afterWrite, when set, runs after each successfully applied write batch
	// (e.g. git push for inGitDB-backed databases). A returned error is
	// reported to the client, but the batch itself is already applied.
	afterWrite func(ctx context.Context) error

	// mu serializes writes: dalgo2ingitdb is single-writer and SQLite is
	// effectively so; reads may run concurrently with each other.
	mu sync.Mutex
}

// Open validates driver/schema-mode compatibility and prepares the database:
// declared collections are provisioned through the driver's
// ddl.SchemaModifier, and (for partial/schemaless modes) the inferred schema
// catalogue is loaded from cataloguePath.
func Open(m *manifest.Manifest, db dal.DB, supportedModes []schema.Mode, cataloguePath string) (*Database, error) {
	mode := m.Database.SchemaMode
	supported := false
	for _, sm := range supportedModes {
		if sm == mode {
			supported = true
			break
		}
	}
	if !supported {
		return nil, &ModeCompatibilityError{Engine: m.Storage.Engine, Requested: mode, Supported: supportedModes}
	}
	d := &Database{Manifest: m, db: db, modes: supportedModes}
	if m.Schemas != nil {
		ctx := context.Background()
		names := make([]string, 0, len(m.Schemas.Collections))
		for name := range m.Schemas.Collections {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := d.ensureCollection(ctx, name, m.Schemas.Collections[name].Fields); err != nil {
				return nil, err
			}
		}
	}
	if mode != schema.ModeStrict {
		cat, err := inferred.Load(cataloguePath)
		if err != nil {
			return nil, fmt.Errorf("failed to load inferred schema catalogue: %w", err)
		}
		d.cat = cat
	}
	return d, nil
}

// ID returns the database id.
func (d *Database) ID() string { return d.Manifest.Database.ID }

// DB exposes the underlying DALgo driver.
func (d *Database) DB() dal.DB { return d.db }

// InferredSnapshot returns the inferred schema catalogue view, or nil for
// strict databases.
func (d *Database) InferredSnapshot() *inferred.Snapshot {
	if d.cat == nil {
		return nil
	}
	return d.cat.Snapshot()
}

// Get returns record data or ErrNotFound.
func (d *Database) Get(ctx context.Context, key *record.Key) (map[string]any, error) {
	data := map[string]any{}
	rec := record.NewRecordWithData(key, data)
	if err := d.db.Get(ctx, rec); err != nil {
		if record.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key.String())
		}
		return nil, err
	}
	if !rec.Exists() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key.String())
	}
	return d.coerceToSchema(key.Collection(), data), nil
}

// coerceToSchema fixes engine type-system drift against the declared schema:
// SQLite stores booleans as 0/1 integers, so declared boolean fields are
// coerced back to bools. Document engines (inGitDB) round-trip natively and
// are unaffected.
func (d *Database) coerceToSchema(collection string, data map[string]any) map[string]any {
	col := d.Manifest.Schemas.Collection(collection)
	if col == nil || data == nil {
		return data
	}
	// Restore declared field-name case. Some engines fold identifiers to a
	// canonical case on write (PostgreSQL lower-cases them), so a record read
	// back can carry differently-cased keys than the schema declares. Rename
	// them to the declared case so downstream validation and clients see
	// faithful field names.
	lowerToDeclared := make(map[string]string, len(col.Fields))
	for name := range col.Fields {
		lowerToDeclared[strings.ToLower(name)] = name
	}
	for key := range data {
		if declared, ok := lowerToDeclared[strings.ToLower(key)]; ok && declared != key {
			data[declared] = data[key]
			delete(data, key)
		}
	}
	for name, f := range col.Fields {
		if f.Type != schema.TypeBoolean {
			continue
		}
		switch v := data[name].(type) {
		case int64:
			data[name] = v != 0
		case float64:
			data[name] = v != 0
		}
	}
	return data
}

// Exists reports whether the record exists.
func (d *Database) Exists(ctx context.Context, key *record.Key) (bool, error) {
	return d.db.Exists(ctx, key)
}

// Collections lists collections known to the driver.
func (d *Database) Collections(ctx context.Context) ([]string, error) {
	reader, ok := dal.As[dbschema.SchemaReader](d.db)
	if !ok {
		return nil, nil
	}
	refs, err := reader.ListCollections(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list collections: %w", err)
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Close is a no-op today: DALgo drivers used by ovdb hold no long-lived
// resources that dal.DB exposes a close for.
func (d *Database) Close() error { return nil }

// Op is one operation of a write batch, in wire format (see docs/api.md).
type Op struct {
	Op      string         `json:"op"` // set | insert | update | delete
	Key     *record.Key    `json:"-"`
	KeyPath string         `json:"key"`
	Data    map[string]any `json:"data,omitempty"`
	Updates []UpdateOp     `json:"updates,omitempty"`
}

// UpdateOp is one field-level update operation, in wire format.
type UpdateOp struct {
	FieldName       string   `json:"fieldName,omitempty"`
	FieldPath       []string `json:"fieldPath,omitempty"`
	Value           any      `json:"value,omitempty"`
	Delete          bool     `json:"delete,omitempty"`
	Transform       string   `json:"transform,omitempty"`
	ServerTimestamp bool     `json:"serverTimestamp,omitempty"`
}

// Apply validates a batch of ops (pre-flight, since inGitDB cannot roll back
// files already written) and then passes them through to the driver in order
// inside one dal.RunReadwriteTransaction — for inGitDB that is at most one
// git commit per batch, with message as the commit message.
func (d *Database) Apply(ctx context.Context, ops []Op, message string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.validateOps(ctx, ops); err != nil {
		return 0, err
	}

	// Partial/schemaless: provision collections the driver hasn't seen, with
	// fields inferred from the first written record (inGitDB requires a
	// definition.yaml per collection; subcollections use path-form names).
	if d.Manifest.Database.SchemaMode != schema.ModeStrict {
		ensured := map[string]bool{}
		for _, op := range ops {
			if op.Data == nil {
				continue
			}
			chains := collectionChains(op.Key)
			for i, chain := range chains {
				if ensured[chain] {
					continue
				}
				var fields map[string]schema.Field
				if i == len(chains)-1 {
					fields = inferFields(op.Data)
				}
				if err := d.ensureCollection(ctx, chain, fields); err != nil {
					return 0, err
				}
				ensured[chain] = true
			}
		}
	}

	if message == "" {
		message = defaultBatchMessage(ops)
	}
	txOpts := []dal.TransactionOption{dal.TxWithMessage(message)}
	err := d.db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		for i, op := range ops {
			var err error
			dk := op.Key
			switch op.Op {
			case "set":
				rec := record.NewRecordWithData(dk, op.Data)
				rec.SetError(nil)
				if err = tx.Set(ctx, rec); err != nil {
					return fmt.Errorf("failed to set %s: %w", op.Key.String(), err)
				}
			case "insert":
				rec := record.NewRecordWithData(dk, op.Data)
				rec.SetError(nil)
				if err = tx.Insert(ctx, rec); err != nil {
					return fmt.Errorf("failed to insert %s: %w", op.Key.String(), err)
				}
			case "update":
				updates, err := toDalUpdates(op.Updates)
				if err != nil {
					return fmt.Errorf("op %d (update %s): %w", i, op.Key.String(), err)
				}
				if err = tx.Update(ctx, dk, updates); err != nil {
					return fmt.Errorf("failed to update %s: %w", op.Key.String(), err)
				}
			case "delete":
				if err = tx.Delete(ctx, dk); err != nil {
					return fmt.Errorf("failed to delete %s: %w", op.Key.String(), err)
				}
			default:
				return fmt.Errorf("op %d: unknown op %q", i, op.Op)
			}
		}
		return nil
	}, txOpts...)
	if err != nil {
		return 0, err
	}

	// Observe written data into the inferred catalogue (partial/schemaless).
	// Updates are not observed in MVP (their final state is inside the
	// driver); roadmap: observe post-update states too.
	if d.cat != nil {
		for _, op := range ops {
			if op.Data != nil {
				if err = d.cat.Observe(op.Key.Collection(), op.Data); err != nil {
					return len(ops), fmt.Errorf("batch applied but inferred-schema observation failed: %w", err)
				}
			}
		}
	}
	if d.afterWrite != nil {
		if err = d.afterWrite(ctx); err != nil {
			return len(ops), fmt.Errorf("batch applied but after-write hook failed: %w", err)
		}
	}
	return len(ops), nil
}

// SetAfterWrite registers a hook invoked after each successfully applied
// write batch. Used by pkg/mount to wire git push policies for inGitDB.
func (d *Database) SetAfterWrite(fn func(ctx context.Context) error) { d.afterWrite = fn }

type stagedState struct {
	data map[string]any // nil when absent/deleted
}

// validateOps simulates the batch against current store state to reject it
// BEFORE any file/row is written: insert conflicts, updates of missing
// records, and (strict/partial) schema validation of every final record
// state. The write mutex makes the read-then-write window safe.
func (d *Database) validateOps(ctx context.Context, ops []Op) error {
	mode := d.Manifest.Database.SchemaMode
	now := time.Now().UTC()
	stage := map[string]*stagedState{}
	leafByKey := map[string]string{}

	load := func(key *record.Key) (*stagedState, error) {
		ks := key.String()
		if st, ok := stage[ks]; ok {
			return st, nil
		}
		st := &stagedState{}
		data, err := d.Get(ctx, key)
		switch {
		case err == nil:
			st.data = data
		case errors.Is(err, ErrNotFound):
		default:
			return nil, err
		}
		stage[ks] = st
		return st, nil
	}

	for i, op := range ops {
		if op.Key == nil {
			return fmt.Errorf("op %d: key is required", i)
		}
		st, err := load(op.Key)
		if err != nil {
			return fmt.Errorf("op %d (%s %s): %w", i, op.Op, op.Key.String(), err)
		}
		leafByKey[op.Key.String()] = op.Key.Collection()
		switch op.Op {
		case "set":
			st.data = deepCopy(op.Data)
		case "insert":
			if st.data != nil {
				return fmt.Errorf("%w: %s", ErrAlreadyExists, op.Key.String())
			}
			st.data = deepCopy(op.Data)
		case "update":
			if st.data == nil {
				return fmt.Errorf("%w: %s", ErrUpdateOfMissingRecord, op.Key.String())
			}
			if err = applyUpdates(st.data, op.Updates, now); err != nil {
				return fmt.Errorf("op %d (update %s): %w", i, op.Key.String(), err)
			}
		case "delete":
			st.data = nil
		default:
			return fmt.Errorf("op %d: unknown op %q", i, op.Op)
		}
	}

	if mode != schema.ModeSchemaless {
		for ks, st := range stage {
			if st.data == nil {
				continue
			}
			leaf := leafByKey[ks]
			if err := schema.ValidateRecord(mode, leaf, d.Manifest.Schemas.Collection(leaf), st.data); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureCollection provisions a collection through the driver's own DDL
// surface (ddl.SchemaModifier). Path-form names ("spaces/ext") declare
// subcollections (supported by dalgo2ingitdb). An empty fields map declares
// a single optional "id" string column: inGitDB requires at least one column,
// and drivers pass undeclared fields through while OpenVaultDB validates
// modes above the driver.
func (d *Database) ensureCollection(ctx context.Context, collection string, fields map[string]schema.Field) error {
	modifier, ok := dal.As[ddl.SchemaModifier](d.db)
	if !ok {
		// Drivers without a DDL surface (Firestore) have implicit
		// collections — nothing to provision; core still validates modes.
		return nil
	}
	// Every collection gets an "id" string primary-key column: relational
	// drivers (dalgo2sqlite) store the record key's ID there; for inGitDB it
	// is advisory metadata. Record data never needs to contain it.
	def := dbschema.CollectionDef{
		Name:       collection,
		Fields:     []dbschema.FieldDef{{Name: "id", Type: dbschema.String, Nullable: false}},
		PrimaryKey: []dal.FieldName{"id"},
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		if name == "id" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def.Fields = append(def.Fields, dbschema.FieldDef{
			Name:     dal.FieldName(name),
			Type:     dbschemaType(fields[name].Type),
			Nullable: !fields[name].Required,
		})
	}
	if err := modifier.CreateCollection(ctx, def, ddl.IfNotExists()); err != nil {
		return fmt.Errorf("failed to ensure collection %q: %w", collection, err)
	}
	return nil
}

// defaultBatchMessage summarizes a batch for stores that version writes
// (inGitDB commits once per batch): clients that pass no transaction message
// still get meaningful history.
func defaultBatchMessage(ops []Op) string {
	counts := map[string]int{}
	collections := map[string]bool{}
	var colNames []string
	for _, op := range ops {
		counts[op.Op]++
		if op.Key != nil {
			col := op.Key.Collection()
			if !collections[col] {
				collections[col] = true
				colNames = append(colNames, col)
			}
		}
	}
	sort.Strings(colNames)
	parts := make([]string, 0, 4)
	for _, verb := range []string{"insert", "set", "update", "delete"} {
		if n := counts[verb]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", verb, n))
		}
	}
	return "ovdb: " + strings.Join(parts, ", ") + " in " + strings.Join(colNames, ", ")
}

func dbschemaType(t schema.FieldType) dbschema.Type {
	switch t {
	case schema.TypeString:
		return dbschema.String
	case schema.TypeNumber:
		return dbschema.Float
	case schema.TypeInteger:
		return dbschema.Int
	case schema.TypeBoolean:
		return dbschema.Bool
	default: // object, array, any — stored as-is; declared as string
		return dbschema.String
	}
}
