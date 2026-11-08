// Recipe: an aggregate function, driven by GROUP BY.
//
//	SELECT category, aggregate.main.int_sum(amount) FROM sales GROUP BY category;
//
// Aggregates keep per-group state across three phases:
//
//   - Update   folds a batch of values into each group's state
//   - Combine  merges partial states for the same group (parallel execution)
//   - Finalize turns each group's state into one output value
//
// State must be serializable (a struct of exported fields, gob-registered)
// because partial states can be combined in a different process.

package main

import (
	"encoding/gob"
	"fmt"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// sumState is the running total for one group.
type sumState struct {
	Total int64
}

func init() { gob.Register(&sumState{}) }

type intSumArgs struct {
	Value int64 `vgi:"pos=0,const=false,doc=Values to add up"`
}

// intSum sums a BIGINT column per group.
type intSum struct{}

var _ vgi.AggregateFunction = (*intSum)(nil)

var intSumSchema = arrow.NewSchema([]arrow.Field{{Name: "result", Type: arrow.PrimitiveTypes.Int64, Nullable: true}}, nil)

func (*intSum) Name() string { return "int_sum" }

// Metadata includes the aggregate semantics the optimizer relies on.
func (*intSum) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description:       "Sum integer values per group",
		Stability:         vgi.StabilityConsistent,
		NullHandling:      vgi.NullHandlingDefault,
		ReturnType:        arrow.PrimitiveTypes.Int64,
		OrderDependent:    vgi.OrderDependenceNotDependent,
		DistinctDependent: vgi.DistinctDependenceNotDependent,
	}
}

func (*intSum) ArgumentSpecs() []vgi.ArgSpec { return vgi.DeriveArgSpecs(intSumArgs{}) }

func (*intSum) OnBind(*vgi.AggregateBindParams) (*vgi.BindResponse, error) {
	return vgi.BindSchema(intSumSchema)
}

func (*intSum) NewState(*vgi.AggregateProcessParams) interface{} { return &sumState{} }

// Update adds a batch of values to each group's total. NULLs are skipped
// without creating the group, so an all-NULL group finalizes to NULL.
func (*intSum) Update(states map[int64]interface{}, gids *vgi.Int64Slice, columns []arrow.Array, _ *vgi.AggregateProcessParams) error {
	if len(columns) == 0 {
		return fmt.Errorf("int_sum: missing value column")
	}
	for i := 0; i < gids.Len(); i++ {
		if columns[0].IsNull(i) {
			continue
		}
		s := vgi.EnsureState(states, gids.At(i), func() *sumState { return &sumState{} })
		s.Total += vgi.GetInt64Value(columns[0], i)
	}
	return nil
}

// Combine merges two partial totals.
func (*intSum) Combine(source, target interface{}, _ *vgi.AggregateProcessParams) (interface{}, error) {
	return &sumState{Total: source.(*sumState).Total + target.(*sumState).Total}, nil
}

// Finalize emits each group's total, in the order given.
func (*intSum) Finalize(gids []int64, states map[int64]interface{}, _ *vgi.AggregateProcessParams) (arrow.RecordBatch, error) {
	b := array.NewInt64Builder(memory.NewGoAllocator())
	defer b.Release()
	for _, gid := range gids {
		if s, ok := states[gid].(*sumState); ok && s != nil {
			b.Append(s.Total)
		} else {
			b.AppendNull()
		}
	}
	arr := b.NewArray()
	defer arr.Release()
	return array.NewRecordBatch(intSumSchema, []arrow.Array{arr}, int64(len(gids))), nil
}

func aggregateWorker() *vgi.Worker {
	w := vgi.NewWorker(vgi.WithCatalogName("aggregate"))
	w.RegisterAggregate(&intSum{})
	return w
}
