// Package hue assembles the example functions into a catalog.
//
// To add a function: write it (see greet.go / sequence.go / fetch_items.go),
// then register it in NewWorker below.
//
// Run it:
//
//	vgi-hue-worker                      # stdio: what DuckDB launches by default
//	vgi-hue-worker --http --port 8000   # HTTP: for Docker / hosted deploys
//	vgi-hue-worker --help
//
// Attach it from DuckDB:
//
//	ATTACH 'hue' (TYPE vgi, LOCATION '/abs/path/to/bin/vgi-hue-worker');
package hue

import (
	"github.com/Query-farm/vgi-go/vgi"
)

// allExamples lists every function's examples for the schema-level tag.
func allExamples() []exampleQuery {
	return exampleQueries(greetExamples, greetWithStyleExamples, sequenceExamples, fetchExamples)
}

// NewWorker builds the worker: one catalog, one main schema. Extra options
// (logging, storage) are appended after the catalog options.
func NewWorker(opts ...vgi.WorkerOption) *vgi.Worker {
	sourceURL, version := SourceURL, Version
	base := []vgi.WorkerOption{
		vgi.WithCatalogName(CatalogName),
		vgi.WithCatalogComment(CatalogDocMD),
		vgi.WithCatalogTags(CatalogTags),
		vgi.WithCatalogInfo(vgi.CatalogInfo{SourceURL: &sourceURL, ImplementationVersion: &version}),
		vgi.WithSchemaComments(map[string]string{"main": SchemaDocMD}),
		vgi.WithSchemaTags(map[string]map[string]string{"main": schemaTags(allExamples())}),
		vgi.WithSecretTypes(APISecretType),
	}
	w := vgi.NewWorker(append(base, opts...)...)
	w.RegisterScalar(vgi.AsScalarFunction[greetArgs](&Greet{}))
	w.RegisterScalar(vgi.AsScalarFunction[greetWithStyleArgs](&GreetWithStyle{}))
	w.RegisterTable(NewSequenceTable())
	w.RegisterTable(NewFetchItems())
	return w
}
