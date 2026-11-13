package hue

import (
	"fmt"
	"math"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// row is one table row keyed by column name. Values: string, bool, float64,
// int32, int64, []string, []any (lists), map[string]any (structs), nil.
type row map[string]any

// columns are rows in column-major form.
type columns map[string][]any

var mem = memory.NewGoAllocator()

// appendValue appends v to builder b (whose type is f.Type).
func appendValue(b array.Builder, v any) error {
	if v == nil {
		b.AppendNull()
		return nil
	}
	switch bb := b.(type) {
	case *array.StringBuilder:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want string, got %T", v)
		}
		bb.Append(s)
	case *array.BooleanBuilder:
		x, ok := v.(bool)
		if !ok {
			return fmt.Errorf("want bool, got %T", v)
		}
		bb.Append(x)
	case *array.Float64Builder:
		x, ok := num(v)
		if !ok {
			return fmt.Errorf("want number, got %T", v)
		}
		bb.Append(x)
	case *array.Int32Builder:
		x, ok := num(v)
		if !ok {
			return fmt.Errorf("want number, got %T", v)
		}
		bb.Append(int32(x))
	case *array.Int64Builder:
		switch x := v.(type) {
		case int64:
			bb.Append(x)
		default:
			f, ok := num(v)
			if !ok {
				return fmt.Errorf("want int64, got %T", v)
			}
			bb.Append(int64(f))
		}
	case *array.TimestampBuilder:
		x, ok := v.(int64)
		if !ok {
			return fmt.Errorf("want int64 micros, got %T", v)
		}
		bb.Append(arrow.Timestamp(x))
	case *array.ListBuilder:
		var xs []any
		switch l := v.(type) {
		case []string:
			for _, s := range l {
				xs = append(xs, s)
			}
		case []any:
			xs = l
		default:
			return fmt.Errorf("want a list, got %T", v)
		}
		bb.Append(true)
		for i, x := range xs {
			if err := appendValue(bb.ValueBuilder(), x); err != nil {
				return fmt.Errorf("element %d: %w", i, err)
			}
		}
	case *array.StructBuilder:
		var r map[string]any
		switch m := v.(type) {
		case row:
			r = m
		case map[string]any:
			r = m
		default:
			return fmt.Errorf("want a struct, got %T", v)
		}
		bb.Append(true)
		st := bb.Type().(*arrow.StructType)
		for i, f := range st.Fields() {
			if err := appendValue(bb.FieldBuilder(i), r[f.Name]); err != nil {
				return fmt.Errorf("%s: %w", f.Name, err)
			}
		}
	default:
		return fmt.Errorf("unsupported builder %T", b)
	}
	return nil
}

// buildBatch builds a batch in schema s from columns (missing columns are NULL).
func buildBatch(s *arrow.Schema, cols columns, n int) (arrow.RecordBatch, error) {
	arrs := make([]arrow.Array, s.NumFields())
	defer func() {
		for _, a := range arrs {
			if a != nil {
				a.Release()
			}
		}
	}()
	for i, f := range s.Fields() {
		b := array.NewBuilder(mem, f.Type)
		vals := cols[f.Name]
		for r := range n {
			var v any
			if r < len(vals) {
				v = vals[r]
			}
			if err := appendValue(b, v); err != nil {
				b.Release()
				return nil, fmt.Errorf("column %s: %w", f.Name, err)
			}
		}
		arrs[i] = b.NewArray()
		b.Release()
	}
	return array.NewRecordBatch(s, arrs, int64(n)), nil
}

// rowsToColumns turns rows into columns over names.
func rowsToColumns(rows []row, names []string) columns {
	out := columns{}
	for _, n := range names {
		vals := make([]any, len(rows))
		for i, r := range rows {
			vals[i] = r[n]
		}
		out[n] = vals
	}
	return out
}

// cellValue reads row i of arr as a Go value (nil for NULL).
func cellValue(arr arrow.Array, i int) any {
	if arr.IsNull(i) {
		return nil
	}
	switch a := arr.(type) {
	case *array.String:
		return a.Value(i)
	case *array.LargeString:
		return a.Value(i)
	case *array.Boolean:
		return a.Value(i)
	case *array.Float64:
		return a.Value(i)
	case *array.Float32:
		return float64(a.Value(i))
	case *array.Int64:
		return a.Value(i)
	case *array.Int32:
		return a.Value(i)
	case *array.Int16:
		return int32(a.Value(i))
	case *array.Int8:
		return int32(a.Value(i))
	case *array.Decimal128:
		dt := a.DataType().(*arrow.Decimal128Type)
		return a.Value(i).ToFloat64(dt.Scale)
	case *array.Timestamp:
		return int64(a.Value(i))
	case *array.List:
		start, end := a.ValueOffsets(i)
		out := make([]any, 0, end-start)
		for j := start; j < end; j++ {
			out = append(out, cellValue(a.ListValues(), int(j)))
		}
		return out
	case *array.LargeList:
		start, end := a.ValueOffsets(i)
		out := make([]any, 0, end-start)
		for j := start; j < end; j++ {
			out = append(out, cellValue(a.ListValues(), int(j)))
		}
		return out
	case *array.Struct:
		st := a.DataType().(*arrow.StructType)
		out := map[string]any{}
		for k, f := range st.Fields() {
			out[f.Name] = cellValue(a.Field(k), i)
		}
		return out
	}
	return arr.ValueStr(i)
}

func column0(batch arrow.RecordBatch, name string) arrow.Array {
	idx := batch.Schema().FieldIndices(name)
	if len(idx) == 0 {
		return nil
	}
	return batch.Column(idx[0])
}

// same compares an input value with a current one the way the TS worker does:
// NULLs, case-insensitive strings, numbers (booleans as 0/1) by value.
func same(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return strings.EqualFold(sa, sb)
		}
		return false
	}
	switch la := a.(type) {
	case []any: // lists element-wise
		lb, ok := b.([]any)
		if !ok || len(la) != len(lb) {
			return false
		}
		for i := range la {
			if !same(la[i], lb[i]) {
				return false
			}
		}
		return true
	case map[string]any: // structs field-wise
		mb, ok := b.(map[string]any)
		if !ok || len(la) != len(mb) {
			return false
		}
		for k, v := range la {
			if !same(v, mb[k]) {
				return false
			}
		}
		return true
	}
	fa, oka := numOrBool(a)
	fb, okb := numOrBool(b)
	if oka && okb {
		return fa == fb
	}
	return false
}

func numOrBool(v any) (float64, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	f, ok := num(v)
	if ok && math.IsNaN(f) {
		return f, false
	}
	return f, ok
}
