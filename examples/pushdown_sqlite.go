// Recipe: push WHERE filters and column selection down into a database.
//
// DuckDB tells a table function which columns the query needs (projection
// pushdown) and which simple predicates it can apply at the source (filter
// pushdown). Turning those into the source's own query means less data
// crosses the wire.
//
//	SELECT id, amount FROM pushdown_sqlite.main.orders() WHERE region = 'west' AND amount > 900;
//
// PushdownFilters.ToSQL translates the filters SQLite can evaluate; anything
// else becomes 1=1 and is left for DuckDB, which re-checks every row it
// receives, so partial pushdown is safe. The demo database is created on first
// use in the system temp directory, with the pure-Go modernc.org/sqlite driver
// the SDK already depends on.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

const ordersPage = 500

var (
	ordersDB     = filepath.Join(os.TempDir(), "vgi_recipe_orders_go.sqlite")
	ordersSchema = arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "amount", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}, nil)
)

// ensureDB creates a deterministic 2,000-row orders table if it does not exist.
func ensureDB() error {
	if _, err := os.Stat(ordersDB); err == nil {
		return nil
	}
	tmp := fmt.Sprintf("%s.%d.tmp", ordersDB, os.Getpid())
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE orders (id INTEGER PRIMARY KEY, region TEXT NOT NULL, amount INTEGER NOT NULL)"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	regions := []string{"north", "south", "east", "west"}
	for i := 1; i <= 2000; i++ {
		if _, err := tx.Exec("INSERT INTO orders VALUES (?, ?, ?)", i, regions[i%4], (i*37)%1000); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, ordersDB); err != nil {
		// Another process won the race (Windows refuses to replace a file
		// that is open); its copy is identical.
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(ordersDB); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// ordersCursor is the keyset pagination cursor: the last id emitted.
type ordersCursor struct {
	LastID int64
}

// orders scans the SQLite orders table, pushing filters and projections into SQL.
type orders struct{}

func (*orders) Name() string { return "orders" }

// Metadata opts in to both kinds of pushdown.
func (*orders) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description:        "Orders from a SQLite database, with filter and projection pushdown",
		Stability:          vgi.StabilityConsistent,
		FilterPushdown:     true,
		ProjectionPushdown: true,
	}
}

func (*orders) ArgumentSpecs() []vgi.ArgSpec { return nil }

func (*orders) OnBind(*vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindSchema(ordersSchema)
}

func (*orders) NewState(*vgi.ProcessParams) (*ordersCursor, error) {
	if err := ensureDB(); err != nil {
		return nil, fmt.Errorf("creating demo database: %w", err)
	}
	return &ordersCursor{}, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Process fetches one page of matching rows, selecting only the projected columns.
func (*orders) Process(ctx context.Context, params *vgi.ProcessParams, state *ordersCursor, out *vgirpc.OutputCollector) error {
	wanted := params.OutputSchema
	cols := []string{"id"}
	for _, f := range wanted.Fields() {
		cols = append(cols, quoteIdent(f.Name))
	}
	where, args := "id > ?", []any{state.LastID}
	if pf := params.CurrentPushdownFilters; pf != nil {
		// ToSQLExact never widens: DuckDB drops every pushed predicate from its
		// plan, so a clause that kept extra rows would return them to the user.
		clause, values, err := pf.ToSQLExact(quoteIdent, "?")
		if err != nil {
			return fmt.Errorf("orders: cannot push filter into SQLite: %w", err)
		}
		if clause != "" {
			where += " AND " + clause
			args = append(args, values...)
		}
	}
	query := fmt.Sprintf("SELECT %s FROM orders WHERE %s ORDER BY id LIMIT %d", strings.Join(cols, ", "), where, ordersPage)
	slog.Debug("sqlite", "sql", query, "args", args)

	db, err := sql.Open("sqlite", "file:"+ordersDB+"?mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	mem := memory.NewGoAllocator()
	builders := make([]array.Builder, wanted.NumFields())
	for i, f := range wanted.Fields() {
		builders[i] = array.NewBuilder(mem, f.Type)
		defer builders[i].Release()
	}
	n := int64(0)
	for rows.Next() {
		var id int64
		vals := make([]sql.NullString, wanted.NumFields())
		dest := []any{&id}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		for i, v := range vals {
			if !v.Valid {
				builders[i].AppendNull()
			} else if err := builders[i].AppendValueFromString(v.String); err != nil {
				return err
			}
		}
		state.LastID = id
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n == 0 {
		return out.Finish()
	}
	arrays := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrays[i] = b.NewArray()
		defer arrays[i].Release()
	}
	return out.EmitArrays(arrays, n)
}

func pushdownSQLiteWorker() *vgi.Worker {
	w := vgi.NewWorker(vgi.WithCatalogName("pushdown_sqlite"))
	w.RegisterTable(vgi.AsTableFunction[ordersCursor](&orders{}))
	return w
}
