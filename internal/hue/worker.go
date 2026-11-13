// Package hue is a Go port of the vgi-hue worker (Philips Hue lights in DuckDB).
package hue

import (
	"github.com/Query-farm/vgi-go/vgi"
)

// NewWorker builds the worker: one catalog, one main schema.
func NewWorker(opts ...vgi.WorkerOption) *vgi.Worker {
	sourceURL, version := SourceURL, Version
	base := []vgi.WorkerOption{
		vgi.WithCatalogName(CatalogName),
		vgi.WithCatalogComment(CatalogDocMD),
		vgi.WithCatalogTags(CatalogTags),
		vgi.WithCatalogInfo(vgi.CatalogInfo{SourceURL: &sourceURL, ImplementationVersion: &version}),
		vgi.WithSchemaComments(map[string]string{"main": SchemaDocMD}),
		vgi.WithSchemaTags(map[string]map[string]string{"main": schemaTags(allExamples())}),
		vgi.WithAttachOptions(attachOptionSpecs()...),
		// Also decides supports_transactions per attach (transactional).
		vgi.WithAttachValidator(attachValidator),
		vgi.WithTransactionHooks(transactionHooks()),
	}
	w := vgi.NewWorker(append(base, opts...)...)
	// Each table registers its scan Function (lights_scan, ...) and lights its
	// UpdateFunction (lights_update). Protocol 3.3 lists them as table_only, so
	// DuckDB has their capabilities (projection pushdown for rowid / UPDATE)
	// without exposing them as SQL functions; registering a scan here with
	// RegisterTable would make it a public SQL function again.
	for _, inv := range inventories {
		w.RegisterCatalogTable("main", inv.catalogTable())
	}
	w.RegisterTable(vgi.AsTableFunction[DoneState](setLightFn{}))
	w.RegisterTable(vgi.AsTableFunction[DoneState](setRoomFn{}))
	w.RegisterTable(vgi.AsTableFunction[DoneState](recallSceneFn{}))
	w.RegisterTable(vgi.AsTableFunction[DoneState](setGradientFn{}))
	w.RegisterTableInOut(vgi.AsTableInOutFunction[NoState](setLightsFn{}))
	return w
}
