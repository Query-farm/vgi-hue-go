// Package hue is a Go port of the vgi-hue worker (Philips Hue lights in DuckDB).
package hue

import (
	"github.com/Query-farm/vgi-go/vgi"
)

func allExamples() []exampleQuery { return nil }

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
	// The lights scan is listed so DuckDB knows it supports projection
	// pushdown, which the rowid virtual column (and so UPDATE) requires.
	w.RegisterTable(vgi.AsTableFunction[DoneState](scanFn{lightsInv}))
	for _, inv := range inventories[1:] {
		w.RegisterTableUnlisted(vgi.AsTableFunction[DoneState](scanFn{inv}))
	}
	// lights carries its UpdateFunction (lights_update), registered with it.
	for _, inv := range inventories {
		w.RegisterCatalogTable("main", inv.catalogTable())
	}
	w.RegisterTable(vgi.AsTableFunction[DoneState](setLightFn{}))
	w.RegisterTable(vgi.AsTableFunction[DoneState](setRoomFn{}))
	w.RegisterTable(vgi.AsTableFunction[DoneState](recallSceneFn{}))
	w.RegisterTableInOut(vgi.AsTableInOutFunction[NoState](setLightsFn{}))
	return w
}
