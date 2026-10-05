package mount

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSQLiteDecimalFunctionsRegisterBeforeOpeningAndPreserveText(t *testing.T) {
	_, path := w1Fixture(t)
	// Mounting initializes the SQLite driver hooks. This later raw handle models
	// the independent sandbox connection and proves callers need no setup step.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var add, multiply, divide, total, average string
	if err := db.QueryRow(`SELECT decimal_add('9007199254740993.1', '0.9'), decimal_mul('12345678901234567890.12', '2'), decimal_div('1', '8', 4), decimal_sum(value), decimal_avg(value, 2) FROM (SELECT '9007199254740993.1' AS value UNION ALL SELECT '0.9')`).Scan(&add, &multiply, &divide, &total, &average); err != nil {
		t.Fatal(err)
	}
	if add != "9007199254740994" || multiply != "24691357802469135780.24" || divide != "0.125" || total != "9007199254740994" || average != "4503599627370497" {
		t.Fatalf("decimal UDF results = %q %q %q %q %q", add, multiply, divide, total, average)
	}
}
