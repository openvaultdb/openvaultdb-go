package joinexec

import (
	"errors"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// exChecked is a source that, like core.Database, says before a transaction whether it
// refuses a document.
type exChecked struct {
	*exSource
	refusal error
	asked   int
}

func (c *exChecked) CheckRead(dal.Query) error {
	c.asked++
	return c.refusal
}

var errExRefused = errors.New("the source refuses the document")

// A document the source refuses is answered with the refusal before any transaction is
// begun, and a document it accepts is read in a transaction as before.
func TestExecuteDatabaseRouteAsksTheSourceBeforeItBeginsATransaction(t *testing.T) {
	q := exJoin(exRef("", "Invoice", "i"), exRef("", "Customer", "c"), true)
	t.Run("refused", func(t *testing.T) {
		mount := &exChecked{exSource: exMount("chinook", "sqlite", false, nil), refusal: errExRefused}
		_, err := exRun(t, q, "chinook", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExRefused) {
			t.Fatalf("err = %v, want the refusal of the source", err)
		}
		if executorCalls, txCalls := mount.counts(); txCalls != 0 || executorCalls != 0 || mount.asked != 1 {
			t.Fatalf("asked %d times, ReadTx %d, Executor %d: want 1, 0, 0", mount.asked, txCalls, executorCalls)
		}
	})
	t.Run("accepted", func(t *testing.T) {
		mount := &exChecked{exSource: exMount("chinook", "sqlite", false, nil)}
		mount.exec.whole = exAnswer(map[string]any{"aid": 1, "bname": "x"})
		res, err := exRun(t, q, "chinook", newExRegistry(mount), exAllow, Limits{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if _, txCalls := mount.counts(); txCalls != 1 || mount.asked != 1 || len(res.Records) != 1 {
			t.Fatalf("asked %d times, ReadTx %d, rows %d: want 1, 1, 1", mount.asked, txCalls, len(res.Records))
		}
	})
}
