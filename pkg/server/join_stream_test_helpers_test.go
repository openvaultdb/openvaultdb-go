package server

import (
	"context"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// legacyJoinStream adapts older handler-test fakes to the stream seam while
// retaining their execution result and error injection.
func legacyJoinStream(execute joinExecuteFunc) joinStreamExecuteFunc {
	return func(ctx context.Context, query dal.StructuredQuery, profile joinexec.Profile, defaultDatabase string, registry joinexec.Registry, authorize joinexec.Authorize, limits joinexec.Limits, emit func(record.Record) error, opts ...joinexec.Option) (joinexec.StreamResult, error) {
		result, err := execute(ctx, query, profile, defaultDatabase, registry, authorize, limits, opts...)
		if err != nil {
			return joinexec.StreamResult{}, err
		}
		for _, row := range result.Records {
			if err := emit(row); err != nil {
				return joinexec.StreamResult{}, err
			}
		}
		return joinexec.StreamResult{Columns: result.Columns, Execution: result.Execution, RowsReturned: len(result.Records)}, nil
	}
}
