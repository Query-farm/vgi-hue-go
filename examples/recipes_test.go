package main

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestEveryRecipeBuilds(t *testing.T) {
	for name, build := range recipes {
		if build() == nil {
			t.Errorf("%s: nil worker", name)
		}
	}
}

func TestJSONLines(t *testing.T) {
	mem := memory.NewGoAllocator()
	ids := array.NewInt64Builder(mem)
	ids.AppendValues([]int64{1, 2}, nil)
	labels := array.NewStringBuilder(mem)
	labels.AppendValues([]string{"a", ""}, []bool{true, false})
	batch := array.NewRecordBatch(arrow.NewSchema([]arrow.Field{
		{Name: "label", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
	}, nil), []arrow.Array{labels.NewArray(), ids.NewArray()}, 2)
	defer batch.Release()
	got, err := jsonLines(batch, true)
	if err != nil || string(got) != "{\"id\":1,\"label\":\"a\"}\n{\"id\":2,\"label\":null}" {
		t.Errorf("jsonLines = %q, %v", got, err)
	}
}
