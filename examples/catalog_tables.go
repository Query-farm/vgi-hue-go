// Recipe: expose a dataset as a named table, with a primary key.
//
// A catalog table is queried by name, with no parentheses, like any DuckDB table:
//
//	SELECT * FROM catalog_tables.main.countries WHERE continent = 'Europe';
//
// Use a table for a fixed or slowly changing dataset (reference data, a
// directory, a lookup list). Use a table function when the caller supplies
// arguments. Declaring NOT NULL and a primary key tells the optimizer, and
// agents, what identifies a row.
//
// Constraints are the worker's own attestation: declare a record as ENFORCED
// with ALL_ROWS validation only if every row the table can return satisfies it.

package main

import (
	"context"
	"log"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
)

var (
	countryCodes      = []string{"DE", "FR", "JP", "KE", "US"}
	countryNames      = []string{"Germany", "France", "Japan", "Kenya", "United States"}
	countryContinents = []string{"Europe", "Europe", "Asia", "Africa", "North America"}

	countriesSchema = arrow.NewSchema([]arrow.Field{
		{Name: "code", Type: arrow.BinaryTypes.String},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "continent", Type: arrow.BinaryTypes.String, Nullable: true},
	}, nil)
)

// countriesState is the batch cursor (exported fields: it must survive HTTP).
type countriesState struct {
	vgi.BatchState
}

// countriesSource emits the rows for the countries table.
type countriesSource struct{}

func (*countriesSource) Name() string { return "countries_source" }

func (*countriesSource) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{Description: "Backing rows for the countries table", Stability: vgi.StabilityConsistent}
}

func (*countriesSource) ArgumentSpecs() []vgi.ArgSpec { return nil }

func (*countriesSource) OnBind(*vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindSchema(countriesSchema)
}

func (*countriesSource) NewState(*vgi.ProcessParams) (*countriesState, error) {
	return &countriesState{BatchState: vgi.NewBatchState(int64(len(countryCodes)), 1024)}, nil
}

func (*countriesSource) Process(_ context.Context, _ *vgi.ProcessParams, state *countriesState, out *vgirpc.OutputCollector) error {
	return vgi.GenerateBatch(&state.BatchState, out, func(size int64) ([]arrow.Array, error) {
		start := state.Index
		pick := func(values []string) arrow.Array {
			return vgi.BuildStringArray(size, func(i int64) string { return values[start+i] })
		}
		return []arrow.Array{pick(countryCodes), pick(countryNames), pick(countryContinents)}, nil
	})
}

// constraint builds one ENFORCED + ALL_ROWS record on main.countries.
func constraint(id string, kind vgi.ConstraintKind, column string) []byte {
	record, err := vgi.ConstraintRecord(vgi.ConstraintRecordSpec{
		SchemaPath: []string{"main"}, Table: "countries", ID: id,
		Kind: kind, Columns: [][]string{{column}},
		Enforcement: vgi.ConstraintEnforced, Validation: vgi.ConstraintAllRows,
	})
	if err != nil {
		log.Fatal(err)
	}
	return record
}

func catalogTablesWorker() *vgi.Worker {
	w := vgi.NewWorker(vgi.WithCatalogName("catalog_tables"))
	w.RegisterCatalogTable("main", vgi.CatalogTable{
		Name:     "countries",
		Comment:  "A small reference list of countries.",
		Columns:  countriesSchema,
		Function: vgi.AsTableFunction[countriesState](&countriesSource{}),
		Constraints: [][]byte{
			constraint("code_not_null", vgi.ConstraintNotNull, "code"),
			constraint("pk", vgi.ConstraintPrimaryKey, "code"),
		},
		ColumnComments: map[string]string{
			"code":      "ISO 3166-1 alpha-2 country code.",
			"name":      "English short name.",
			"continent": "Continent the country is in.",
		},
	})
	return w
}
