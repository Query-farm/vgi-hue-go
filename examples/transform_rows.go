// Recipe: transform the rows of a query with a table in-out function.
//
// A table in-out function receives another query's rows, batch by batch, and
// emits rows back. Use it to clean, enrich, filter or reshape data in SQL:
//
//	SELECT * FROM transform_rows.main.positive_only((SELECT * FROM readings));
//
// Here: keep only rows whose value column is greater than zero. The output
// schema equals the input schema, so it works on any table that has a value
// column.

package main

import (
	"context"
	"fmt"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/compute"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// positiveOnlyState is empty: each batch is decided on its own.
type positiveOnlyState struct{}

// positiveOnly emits the input rows whose value is greater than zero.
type positiveOnly struct{}

func (*positiveOnly) Name() string { return "positive_only" }

func (*positiveOnly) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description: "Keep the rows of a query whose value column is positive",
		Stability:   vgi.StabilityConsistent,
	}
}

// ArgumentSpecs declares the input relation. A TABLE argument is an explicit
// ArgSpec: it arrives as the stream of batches Process is called with.
func (*positiveOnly) ArgumentSpecs() []vgi.ArgSpec {
	return []vgi.ArgSpec{{Name: "data", Position: 0, ArrowType: "table",
		Doc: "Rows to filter; must have a numeric value column."}}
}

// OnBind makes the output columns the input columns, and fails early without a value column.
func (*positiveOnly) OnBind(params *vgi.BindParams) (*vgi.BindResponse, error) {
	if params.InputSchema == nil || len(params.InputSchema.FieldIndices("value")) == 0 {
		return nil, fmt.Errorf("positive_only needs an input column named 'value'")
	}
	return vgi.BindSchema(params.InputSchema)
}

func (*positiveOnly) NewState(*vgi.ProcessParams) (*positiveOnlyState, error) {
	return &positiveOnlyState{}, nil
}

// positive reports whether row i of a numeric column is > 0 (NULL is not).
func positive(col arrow.Array, i int) bool {
	if col.IsNull(i) {
		return false
	}
	switch a := col.(type) {
	case *array.Int8:
		return a.Value(i) > 0
	case *array.Int16:
		return a.Value(i) > 0
	case *array.Int32:
		return a.Value(i) > 0
	case *array.Int64:
		return a.Value(i) > 0
	case *array.Float32:
		return a.Value(i) > 0
	case *array.Float64:
		return a.Value(i) > 0
	default:
		return false
	}
}

// Process filters each incoming batch and emits the survivors.
func (*positiveOnly) Process(ctx context.Context, _ *vgi.ProcessParams, _ *positiveOnlyState,
	batch arrow.RecordBatch, out *vgirpc.OutputCollector,
) error {
	value := batch.Column(batch.Schema().FieldIndices("value")[0])
	mask := array.NewBooleanBuilder(memory.NewGoAllocator())
	defer mask.Release()
	for i := 0; i < value.Len(); i++ {
		mask.Append(positive(value, i))
	}
	keep := mask.NewArray()
	defer keep.Release()
	kept, err := compute.FilterRecordBatch(ctx, batch, keep, compute.DefaultFilterOptions())
	if err != nil {
		return err
	}
	defer kept.Release()
	return out.Emit(kept)
}

// Finalize has nothing to flush: a streaming filter holds nothing back.
func (*positiveOnly) Finalize(context.Context, *vgi.ProcessParams, *positiveOnlyState) ([]arrow.RecordBatch, error) {
	return nil, nil
}

func transformRowsWorker() *vgi.Worker {
	w := vgi.NewWorker(vgi.WithCatalogName("transform_rows"))
	w.RegisterTableInOut(vgi.AsTableInOutFunction[positiveOnlyState](&positiveOnly{}))
	return w
}
