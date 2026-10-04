package core_test

import (
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// *core.Database is a join source. The assertion is in this external test
// package so that pkg/core never imports pkg/joinexec.
var _ joinexec.Source = (*core.Database)(nil)
