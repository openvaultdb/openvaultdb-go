package joinexec

import (
	"context"
	"errors"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// A source with access policies serves no schema through JoinFields: its executor
// is not asked, whether the executor would answer fields, fail or refuse the
// collection, so the answer for a collection the source does not declare comes
// where a policy decision comes, at the read, and not from a question that
// precedes it. The request authorisation of the source still applies first.
func TestLeafJoinFieldsOfASourceWithAccessPoliciesDoesNotAskTheExecutor(t *testing.T) {
	for name, exec := range map[string]*fakeExecutor{
		"an executor that has fields": {fields: []string{"id", "name"}},
		"an executor that fails":      {fieldsErr: errBoom},
		"an executor that refuses":    {fieldsErr: errors.New("collection is not declared")},
	} {
		t.Run(name, func(t *testing.T) {
			guard := NewGuard(allowAll, Limits{})
			source := &fakeSource{id: "vault", engine: "sqlite", policies: true, exec: fieldsExecutor{exec}}
			provider := guard.Leaf(source).(dal.JoinFieldsProvider)
			fields, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("vault", "", "A", "a"))
			if err != nil || fields != nil {
				t.Fatalf("fields=%v err=%v", fields, err)
			}
			if exec.fieldCalls != 0 || exec.queryCalls != 0 || guard.Err() != nil {
				t.Fatalf("the executor was asked: fields %d, reads %d, guard %v", exec.fieldCalls, exec.queryCalls, guard.Err())
			}
		})
	}
	t.Run("a collection the request may not read is still refused", func(t *testing.T) {
		exec := &fakeExecutor{fields: []string{"id"}}
		guard := NewGuard(func(string, string) bool { return false }, Limits{})
		source := &fakeSource{id: "vault", engine: "sqlite", policies: true, exec: fieldsExecutor{exec}}
		provider := guard.Leaf(source).(dal.JoinFieldsProvider)
		_, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("vault", "", "A", "a"))
		var denied *SourceDeniedError
		if !errors.As(err, &denied) || exec.fieldCalls != 0 {
			t.Fatalf("err = %v, field calls %d", err, exec.fieldCalls)
		}
	})
}
