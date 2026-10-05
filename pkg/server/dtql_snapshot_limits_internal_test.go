package server

import (
	"strings"
	"testing"
)

func TestDefaultSnapshotLimitsAreTheHistoricalValues(t *testing.T) {
	want := SnapshotLimits{Slots: 2, Bytes: 512 << 20, Rows: 1_000_000}
	if got := DefaultSnapshotLimits(); got != want {
		t.Fatalf("DefaultSnapshotLimits() = %+v, want %+v", got, want)
	}
	if got := New("test", nil).snapshotLimits; got != want {
		t.Fatalf("server default limits = %+v, want %+v", got, want)
	}
}

func TestWithSnapshotLimitsSetsLimits(t *testing.T) {
	limits := SnapshotLimits{Slots: 1, Bytes: 1 << 20, Rows: 10}
	if got := New("test", nil, WithSnapshotLimits(limits)).snapshotLimits; got != limits {
		t.Fatalf("limits = %+v, want %+v", got, limits)
	}
}

func TestWithSnapshotLimitsRefusesNonPositiveValues(t *testing.T) {
	good := SnapshotLimits{Slots: 1, Bytes: 1, Rows: 1}
	cases := map[string]SnapshotLimits{
		"zero slots":     {Slots: 0, Bytes: 1, Rows: 1},
		"negative slots": {Slots: -1, Bytes: 1, Rows: 1},
		"zero bytes":     {Slots: good.Slots, Bytes: 0, Rows: 1},
		"negative bytes": {Slots: good.Slots, Bytes: -1, Rows: 1},
		"zero rows":      {Slots: good.Slots, Bytes: 1, Rows: 0},
		"negative rows":  {Slots: good.Slots, Bytes: 1, Rows: -1},
		"zero value":     {},
	}
	for name, limits := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				if !strings.Contains(msg, "WithSnapshotLimits") {
					t.Fatalf("panic = %q, want one naming WithSnapshotLimits", msg)
				}
			}()
			New("test", nil, WithSnapshotLimits(limits))
			t.Fatal("expected a refusal")
		})
	}
}
