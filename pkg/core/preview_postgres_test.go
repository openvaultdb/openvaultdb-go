package core

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dal-go/record"
)

const previewSwitch = "OVDB_PREVIEW_POSTGRES_QUERIES"

// setPreview sets the preview switch for the test, or removes it when set is
// false, and restores the environment afterwards.
func setPreview(t *testing.T, set bool, value string) {
	t.Helper()
	t.Setenv(previewSwitch, value)
	if !set {
		if err := os.Unsetenv(previewSwitch); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAPostgresMountAnswersQueriesOnlyWhenThePreviewSwitchIsOn: PostgreSQL joins
// the allow-list of structured queries only when the environment of the server
// holds OVDB_PREVIEW_POSTGRES_QUERIES=1, and nothing but the value 1 turns it on.
// Without it a PostgreSQL mount answers as it always has: every route that
// hands a structured query to the driver refuses it with *QueryUnsupportedError,
// and the driver is not reached. MySQL stays refused whatever the switch says, and
// the engines that were cleared before stay cleared without it.
func TestAPostgresMountAnswersQueriesOnlyWhenThePreviewSwitchIsOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   bool
		value string
		on    bool
	}{
		{"not set", false, "", false},
		{"empty", true, "", false},
		{"0", true, "0", false},
		{"true", true, "true", false},
		{"yes", true, "yes", false},
		{"11", true, "11", false},
		{"1 and a space", true, "1 ", false},
		{"a space and 1", true, " 1", false},
		{"1", true, "1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			setPreview(t, c.set, c.value)
			for engine, want := range map[string]bool{
				"postgres":  c.on,
				"mysql":     false,
				"sqlite":    true,
				"ingitdb":   true,
				"firestore": true,
				"oracle":    false,
			} {
				db, fake := openEngine(t, engine)
				if got := db.CanQuery(); got != want {
					t.Errorf("%s: CanQuery = %v, want %v", engine, got, want)
				}
				err := db.guardQuery()
				if (err == nil) != want {
					t.Errorf("%s: guardQuery = %v, want a refusal %v", engine, err, !want)
				}
				_, err = db.Execute(context.Background(), Query{Collection: "customers"})
				if want && fake.queries != 1 {
					t.Errorf("%s: Execute = %v with %d driver calls, want the driver to be reached once", engine, err, fake.queries)
				}
				var refusal *QueryUnsupportedError
				if !want && (!errors.As(err, &refusal) || refusal.Engine != engine || fake.queries != 0) {
					t.Errorf("%s: Execute = %v with %d driver calls, want the refusal naming the engine and no call", engine, err, fake.queries)
				}
			}
		})
	}
}

// TestThePreviewSwitchIsReadOnceWhenTheMountOpens: a mount answers by the value the
// switch had when it opened. Changing the environment later changes nothing for
// it, and a mount that opens afterwards reads the new value.
func TestThePreviewSwitchIsReadOnceWhenTheMountOpens(t *testing.T) {
	setPreview(t, true, "1")
	on, _ := openEngine(t, "postgres")
	setPreview(t, true, "0")
	off, _ := openEngine(t, "postgres")
	if !on.CanQuery() || off.CanQuery() {
		t.Fatalf("after the switch was turned off: first mount %v, second mount %v, want true and false", on.CanQuery(), off.CanQuery())
	}
	setPreview(t, false, "")
	if !on.CanQuery() || off.CanQuery() {
		t.Fatalf("after the switch was removed: first mount %v, second mount %v, want true and false", on.CanQuery(), off.CanQuery())
	}
	setPreview(t, true, "1")
	if !on.CanQuery() || off.CanQuery() {
		t.Fatalf("after the switch was turned on: first mount %v, second mount %v, want true and false", on.CanQuery(), off.CanQuery())
	}
}

// TestKeyReadsAndWritesOfAPostgresMountDoNotFollowThePreviewSwitch: the switch is
// about structured queries. A key read and a write answer the same with it on and
// off.
func TestKeyReadsAndWritesOfAPostgresMountDoNotFollowThePreviewSwitch(t *testing.T) {
	for _, value := range []string{"", "1"} {
		setPreview(t, true, value)
		db, fake := writeGuardOpen(t, "postgres", "customers")
		key := record.NewKeyWithID("customers", "c1")
		if _, err := db.Get(context.Background(), key); err != nil {
			t.Fatalf("switch %q: Get: %v", value, err)
		}
		if _, err := db.Apply(context.Background(), []Op{{Op: "set", Key: key, Data: map[string]any{"name": "Ada"}}}, ""); err != nil {
			t.Fatalf("switch %q: Apply: %v", value, err)
		}
		if fake.gets == 0 || fake.transactions == 0 {
			t.Fatalf("switch %q: the driver was not reached: gets %d, transactions %d", value, fake.gets, fake.transactions)
		}
	}
}
