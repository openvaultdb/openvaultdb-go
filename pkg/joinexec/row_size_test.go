package joinexec

import (
	"math"
	"testing"
)

func TestEncodeRowMeasuresSpecialFloatsAsTheirSpelling(t *testing.T) {
	encoded, err := encodeRow(map[string]any{
		"nan":     math.NaN(),
		"inf":     math.Inf(1),
		"ninf":    float32(math.Inf(-1)),
		"finite":  1.5,
		"single":  float32(2),
		"list":    []any{math.NaN(), "x"},
		"float64": []float64{math.Inf(1), 1},
		"float32": []float32{float32(math.NaN()), 2},
		"nested":  map[string]any{"v": math.NaN()},
		"text":    "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"finite":1.5,"float32":["NaN",2],"float64":["Infinity",1],"inf":"Infinity","list":["NaN","x"],"nan":"NaN","nested":{"v":"NaN"},"ninf":"-Infinity","single":2,"text":"t"}`
	if string(encoded) != want {
		t.Fatalf("encoded = %s, want %s", encoded, want)
	}
	if _, err := encodeRow(map[string]any{"c": make(chan int)}); err == nil {
		t.Fatal("a value JSON cannot encode at all was measured")
	}
	type wrapped struct{ F float64 }
	if _, err := encodeRow(map[string]any{"s": wrapped{F: math.NaN()}}); err == nil {
		t.Fatal("a NaN the copy does not reach was measured")
	}
}
