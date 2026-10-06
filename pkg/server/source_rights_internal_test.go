package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

func TestSourceRightsUnexpectedExecutedSourceFailsBeforeOutput(t *testing.T) {
	db := immutableProfileFixture(t, "db", false)
	service := New("test", map[string]*core.Database{"db": db}, WithSourceRights("stable-server", &license.Declaration{Text: "Source terms"}))
	t.Cleanup(service.CloseSnapshots)
	service.joinExecute = func(context.Context, dal.StructuredQuery, joinexec.Profile, string, joinexec.Registry, joinexec.Authorize, joinexec.Limits, ...joinexec.Option) (joinexec.Result, error) {
		return joinexec.Result{Execution: joinexec.Execution{Sources: []joinexec.ExecutionSource{{Database: "db", Collection: "unexpected"}}}}, nil
	}
	response := profileRequest(service.Handler(), "POST", "/v1/databases/db/dtql", "from: {name: things, alias: t}\ncolumns: [{field: id, source: t}]\n", nil)
	if response.Code != 422 || strings.Contains(response.Body.String(), "unexpected") || strings.Contains(response.Body.String(), "sourceRights") || strings.Contains(response.Body.String(), "records") {
		t.Fatal(response.Code, response.Body.String())
	}
}
func TestSourceRightsCountsAgainstWholeResponseBudget(t *testing.T) {
	db := immutableProfileFixture(t, "db", false)
	service := New("test", map[string]*core.Database{"db": db}, WithSourceRights("stable-server", &license.Declaration{Text: strings.Repeat("t", 65536)}))
	t.Cleanup(service.CloseSnapshots)
	capture, err := service.singleRights(db, "things")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	rows := map[string]any{"records": []any{map[string]any{"data": strings.Repeat("x", core.ResultBufferBytes-1024)}}}
	service.writeRightsResult(response, httptest.NewRequest(http.MethodGet, "/", nil), rows, capture, capture.allUsed(), core.ResultBufferBytes)
	if response.Code != 422 || strings.Contains(response.Body.String(), "sourceRights") || strings.Contains(response.Body.String(), "records") {
		t.Fatal(response.Code, response.Body.String())
	}
}
