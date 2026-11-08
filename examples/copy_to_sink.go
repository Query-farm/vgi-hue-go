// Recipe: write query results out with COPY … TO and a custom format.
//
//	ATTACH 'copy_to_sink' (TYPE vgi, LOCATION '/abs/bin/recipes copy_to_sink');
//	COPY (SELECT * FROM t) TO 'out.jsonl' (FORMAT 'copy_to_sink.jsonl_out');
//
// The format name is qualified by the attach alias. Write runs once per batch
// (possibly in parallel, possibly in different processes), so it stashes rows
// in execution-scoped storage; Close runs once and writes the file. Swap the
// file write for an upload to object storage, a warehouse load, or an API call.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
)

// shardKey is the state-log key each batch's lines are appended under.
var shardKey = []byte("shards")

// writeOptions are the COPY options: COPY … TO … (FORMAT 'copy_to_sink.jsonl_out', sort_keys true).
type writeOptions struct {
	SortKeys bool `vgi:"default=false,doc=Sort the keys of each JSON object"`
}

// writeJSONLines writes each row as one JSON object per line.
type writeJSONLines struct{}

var _ vgi.CopyToFunction = (*writeJSONLines)(nil)

func (*writeJSONLines) Name() string { return "write_json_lines" }

func (*writeJSONLines) CopyToFormat() string { return "jsonl_out" }

func (*writeJSONLines) CopyToComment() string { return "Newline-delimited JSON, one object per row" }

func (*writeJSONLines) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{Description: "Write each row as one JSON object per line", Stability: vgi.StabilityConsistent}
}

func (*writeJSONLines) ArgumentSpecs() []vgi.ArgSpec { return vgi.DeriveArgSpecs(writeOptions{}) }

// jsonLines renders a batch as newline-separated JSON objects, keys in column
// order (or sorted).
func jsonLines(batch arrow.RecordBatch, sortKeys bool) ([]byte, error) {
	order := make([]int, batch.NumCols())
	for i := range order {
		order[i] = i
	}
	if sortKeys {
		sort.Slice(order, func(a, b int) bool { return batch.ColumnName(order[a]) < batch.ColumnName(order[b]) })
	}
	var buf bytes.Buffer
	for r := 0; r < int(batch.NumRows()); r++ {
		if r > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteByte('{')
		for n, c := range order {
			if n > 0 {
				buf.WriteByte(',')
			}
			key, _ := json.Marshal(batch.ColumnName(c))
			val, err := json.Marshal(batch.Column(c).GetOneForMarshal(r))
			if err != nil {
				return nil, err
			}
			buf.Write(key)
			buf.WriteByte(':')
			buf.Write(val)
		}
		buf.WriteByte('}')
	}
	return buf.Bytes(), nil
}

// Write serializes this batch and stashes it (scoped to this COPY).
func (*writeJSONLines) Write(_ context.Context, params *vgi.ProcessParams, batch arrow.RecordBatch) error {
	var opts writeOptions
	if err := vgi.BindArgs(params.Args, &opts); err != nil {
		return err
	}
	if batch.NumRows() == 0 {
		return nil
	}
	lines, err := jsonLines(batch, opts.SortKeys)
	if err != nil {
		return err
	}
	_, err = params.Storage.StateAppend(shardKey, lines)
	return err
}

// Close writes every stashed batch to the file once.
func (*writeJSONLines) Close(_ context.Context, params *vgi.ProcessParams) error {
	if params.CopyTo == nil {
		return fmt.Errorf("jsonl_out: missing COPY TO context")
	}
	entries, err := params.Storage.StateLogScan(shardKey, -1, 0)
	if err != nil {
		return err
	}
	f, err := os.Create(params.CopyTo.FilePath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := f.Write(append(e.Value, '\n')); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

func copyToSinkWorker() *vgi.Worker {
	w := vgi.NewWorker(vgi.WithCatalogName("copy_to_sink"))
	w.RegisterCopyTo(&writeJSONLines{})
	return w
}
