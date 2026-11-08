// Recipe: let DuckDB cache an expensive result.
//
// The worker *advertises* how long a result stays fresh. DuckDB caches it, and
// repeat queries inside the TTL never reach the worker. After the TTL, DuckDB
// revalidates with an ETag; if nothing changed the worker answers "not
// modified" with zero rows.
//
//	SELECT * FROM cache_expensive_call.main.exchange_rates();

package main

import (
	"context"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// dataVersion is the only thing revalidation compares. In a real worker derive
// it from the upstream: a Last-Modified header, a version column, a hash.
const dataVersion = `"rates-v1"`

var (
	ratesSchema = arrow.NewSchema([]arrow.Field{
		{Name: "currency", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "rate", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}, nil)
	currencies = []string{"EUR", "GBP", "JPY"}
	rates      = []float64{1.09, 1.27, 0.0067}
)

// ratesState records whether the single batch has been emitted.
type ratesState struct {
	Done bool
}

// exchangeRates returns rates from a slow upstream, cacheable for five minutes.
type exchangeRates struct{}

func (*exchangeRates) Name() string { return "exchange_rates" }

func (*exchangeRates) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description: "Exchange rates from a slow upstream, cacheable for 5 minutes",
		Stability:   vgi.StabilityConsistent,
	}
}

func (*exchangeRates) ArgumentSpecs() []vgi.ArgSpec { return nil }

func (*exchangeRates) OnBind(*vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindSchema(ratesSchema)
}

func (*exchangeRates) NewState(*vgi.ProcessParams) (*ratesState, error) { return &ratesState{}, nil }

func ratesBatch(rows bool) arrow.RecordBatch {
	mem := memory.NewGoAllocator()
	names := array.NewStringBuilder(mem)
	defer names.Release()
	vals := array.NewFloat64Builder(mem)
	defer vals.Release()
	n := 0
	if rows {
		names.AppendValues(currencies, nil)
		vals.AppendValues(rates, nil)
		n = len(currencies)
	}
	return array.NewRecordBatch(ratesSchema, []arrow.Array{names.NewArray(), vals.NewArray()}, int64(n))
}

// Process emits the rates with cache metadata, or confirms the client's copy is current.
func (*exchangeRates) Process(_ context.Context, params *vgi.ProcessParams, state *ratesState, out *vgirpc.OutputCollector) error {
	if state.Done {
		return out.Finish()
	}
	state.Done = true
	if params.IfNoneMatch != nil && *params.IfNoneMatch == dataVersion {
		empty := ratesBatch(false)
		defer empty.Release()
		if err := vgi.Emit(out, empty, vgi.WithCacheControl(&vgi.CacheControl{
			Ttl: vgi.Seconds(300), ETag: dataVersion, Revalidatable: true, NotModified: true,
		})); err != nil {
			return err
		}
		return out.Finish()
	}
	batch := ratesBatch(true)
	defer batch.Release()
	// Cache metadata must ride on the FIRST batch.
	if err := vgi.Emit(out, batch, vgi.WithCacheControl(&vgi.CacheControl{
		Ttl:                  vgi.Seconds(300),
		ETag:                 dataVersion,
		Revalidatable:        true,
		StaleWhileRevalidate: vgi.Seconds(60),
		StaleIfError:         vgi.Seconds(600),
	})); err != nil {
		return err
	}
	return out.Finish()
}

func cacheExpensiveCallWorker() *vgi.Worker {
	w := vgi.NewWorker(vgi.WithCatalogName("cache_expensive_call"))
	w.RegisterTable(vgi.AsTableFunction[ratesState](&exchangeRates{}))
	return w
}
