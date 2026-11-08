// Table function example: sequence_table.
//
// A table function returns a set of rows. This one is a deterministic
// generator, the simplest shape to learn from:
//
//   - count is positional and required; step is *named* with a default, so
//     callers write sequence_table(5) or sequence_table(5, step := 2).
//     (Named arguments work on table functions, not on scalars.)
//   - ge/le constraints are enforced before the function runs and are
//     published so agents know the valid range.
//   - Output streams in bounded batches. SequenceState remembers where the
//     last batch stopped. Over HTTP each batch can be served by a different
//     request (or process), so the SDK gob-encodes the state into the
//     continuation token: keep the cursor in *exported* fields, because gob
//     silently drops unexported ones and the scan would restart from row 0.

package starter

import (
	"context"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
)

// BatchRows caps the rows emitted per batch.
const BatchRows = 10_000

// sequenceArgs declares sequence_table(count, step := 1).
type sequenceArgs struct {
	Count int64 `vgi:"pos=0,ge=0,le=1000000,doc=How many rows to generate, from 0 to 1,000,000."`
	Step  int64 `vgi:"default=1,ge=1,doc=Gap between consecutive n values; at least 1."`
}

// SequenceState is the serializable cursor: how many rows have been emitted.
type SequenceState struct {
	Emitted int64
	Count   int64
	Step    int64
}

var sequenceSchema = arrow.NewSchema([]arrow.Field{
	column("n", arrow.PrimitiveTypes.Int64, "The generated value: row index times step.", false),
	column("squared", arrow.PrimitiveTypes.Int64, "n multiplied by itself.", false),
}, nil)

var sequenceExamples = []vgi.CatalogExample{
	{SQL: "SELECT n, squared FROM starter.main.sequence_table(5) ORDER BY n",
		Description: "The first five values and their squares"},
	{SQL: "SELECT n FROM starter.main.sequence_table(4, step := 10) ORDER BY n",
		Description: "Four values stepping by ten"},
}

// SequenceTable generates count rows of (n, squared) with n = 0, step, 2*step, ...
type SequenceTable struct{}

var _ vgi.TypedTableFunc[SequenceState] = (*SequenceTable)(nil)

// Name is the SQL name.
func (*SequenceTable) Name() string { return "sequence_table" }

// Metadata describes the function to DuckDB, agents and vgi-lint.
func (*SequenceTable) Metadata() vgi.FunctionMetadata {
	tags := objectTags(docs{
		Title:    "Sequence Table",
		Category: "generators",
		Keywords: "sequence, series, range, generate rows, numbers, squares",
		DocLLM: "Generate a deterministic table of count rows with columns n and squared, " +
			"where n runs 0, step, 2*step, and so on. count is positional (0 to 1,000,000); " +
			"step is a named argument that defaults to 1. Rows stream back in batches, so " +
			"large counts do not buffer in memory.",
		DocMD: "## sequence_table(count, step := 1)\n\n" +
			"count rows of (n, squared) with n = 0, step, 2*step, ...\n\n" +
			"Deterministic, so it is handy for tests and demos.",
	})
	tags["vgi.result_columns_schema"] = mustJSON([]resultColumn{
		{Name: "n", Type: "BIGINT", Description: "The generated value: row index times step."},
		{Name: "squared", Type: "BIGINT", Description: "n multiplied by itself."},
	})
	tags["vgi.executable_examples"] = mustJSON([]map[string]any{
		{
			"description":     "Three rows stepping by two",
			"sql":             "SELECT n, squared FROM starter.main.sequence_table(3, step := 2) ORDER BY n",
			"expected_result": [][]int64{{0, 0}, {2, 4}, {4, 16}},
		},
		{
			"description":     "Count the rows generated",
			"sql":             "SELECT count(*) AS rows FROM starter.main.sequence_table(1000)",
			"expected_result": [][]int64{{1000}},
		},
	})
	return vgi.FunctionMetadata{
		Description: "Generate count rows of (n, squared), stepping n by step",
		Stability:   vgi.StabilityConsistent,
		Tags:        withExamples(tags, sequenceExamples),
		Examples:    sequenceExamples,
	}
}

// ArgumentSpecs derives the signature from sequenceArgs' tags.
func (*SequenceTable) ArgumentSpecs() []vgi.ArgSpec { return vgi.DeriveArgSpecs(sequenceArgs{}) }

// OnBind declares the fixed output schema.
func (*SequenceTable) OnBind(_ *vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindSchema(sequenceSchema)
}

// Cardinality tells the planner exactly how many rows to expect.
func (*SequenceTable) Cardinality(params *vgi.BindParams) (*vgi.TableCardinality, error) {
	var args sequenceArgs
	if err := vgi.BindArgs(params.Args, &args); err != nil {
		return nil, err
	}
	return &vgi.TableCardinality{Estimate: args.Count, Max: args.Count}, nil
}

// NewState starts the cursor at zero. Arguments are copied into the state so
// Process never re-parses them.
func (*SequenceTable) NewState(params *vgi.ProcessParams) (*SequenceState, error) {
	var args sequenceArgs
	if err := vgi.BindArgs(params.Args, &args); err != nil {
		return nil, err
	}
	return &SequenceState{Count: args.Count, Step: args.Step}, nil
}

// Process emits one bounded batch per call and finishes when count rows are out.
func (*SequenceTable) Process(_ context.Context, _ *vgi.ProcessParams, state *SequenceState, out *vgirpc.OutputCollector) error {
	if state.Emitted >= state.Count {
		return out.Finish()
	}
	rows := min(int64(BatchRows), state.Count-state.Emitted)
	start, step := state.Emitted, state.Step
	n := vgi.BuildInt64Array(rows, func(i int64) int64 { return (start + i) * step })
	defer n.Release()
	sq := vgi.BuildInt64Array(rows, func(i int64) int64 { v := (start + i) * step; return v * v })
	defer sq.Release()
	state.Emitted += rows
	return out.EmitArrays([]arrow.Array{n, sq}, rows)
}

// NewSequenceTable returns the registration-ready function.
func NewSequenceTable() vgi.TableFunction {
	return vgi.AsTableFunction[SequenceState](&SequenceTable{})
}
