// Discovery metadata: the one file to edit when you rename or extend the worker.
//
// DuckDB, coding agents and the vgi-lint metadata checker all read these tags.
// Keep every object documented and `make lint-explain` stays at 100.
//
//   - Catalog-level tags (title, license, author, links, long description)
//     live in CatalogTags below.
//   - Every function carries objectTags(...): a title, a category from
//     SchemaCategories, search keywords, and LLM- and human-facing docs.

package hue

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
)

// CatalogName is the name DuckDB attaches the worker under.
const CatalogName = "hue"

// SourceURL is where this worker's code lives.
const SourceURL = "https://github.com/your-org/vgi-hue"

// category is one entry of the schema's vgi.categories list.
type category struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// SchemaCategories group functions in docs and agent tools. Every function's
// vgi.category must name one of these.
var SchemaCategories = []category{
	{Name: "inventory", Title: "Inventory", Description: "Lights, rooms, zones and scenes on the bridge."},
	{Name: "sensors", Title: "Sensors", Description: "Motion, temperature, light-level, contact and battery readings."},
	{Name: "control", Title: "Control", Description: "Functions that change the state of real lights."},
}

// mustJSON marshals v, panicking on failure (only static data is passed).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// keywordsArray turns "a, b, c" into the JSON array string vgi.keywords requires.
func keywordsArray(keywords string) string {
	out := []string{}
	for _, k := range strings.Split(keywords, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return mustJSON(out)
}

// docs is the set of standard per-object discovery fields.
type docs struct {
	Title    string
	Category string
	Keywords string
	DocLLM   string
	DocMD    string
}

// objectTags builds the standard per-object discovery tags.
func objectTags(d docs) map[string]string {
	known := false
	for _, c := range SchemaCategories {
		known = known || c.Name == d.Category
	}
	if !known {
		panic(fmt.Sprintf("unknown category %q; add it to SchemaCategories", d.Category))
	}
	return map[string]string{
		"vgi.title":    d.Title,
		"vgi.category": d.Category,
		"vgi.keywords": keywordsArray(d.Keywords),
		"vgi.doc_llm":  d.DocLLM,
		"vgi.doc_md":   d.DocMD,
	}
}

// resultColumn describes one output column for vgi.result_columns_schema.
type resultColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// column builds an Arrow field whose comment shows up in DuckDB's DESCRIBE and agent tools.
func column(name string, typ arrow.DataType, comment string, nullable bool) arrow.Field {
	return arrow.Field{Name: name, Type: typ, Nullable: nullable,
		Metadata: arrow.NewMetadata([]string{"comment"}, []string{comment})}
}

// exampleQuery is one entry of vgi.example_queries.
type exampleQuery struct {
	Description string `json:"description"`
	SQL         string `json:"sql"`
}

const catalogDocLLM = "An example VGI worker generated from the Query.Farm starter template. It shows the three " +
	"shapes most workers need: a vectorized scalar function (`greet`, with a one- and two-argument " +
	"overload), a deterministic table function with a named default argument (`sequence_table`), " +
	"and a table function that pages through a REST API with retries and an optional bearer token " +
	"taken from a DuckDB secret or an environment variable (`fetch_items`). Replace these with " +
	"your own functions; keep the metadata complete so agents can discover and call them."

// CatalogDocMD is the catalog's long human-facing description (also its comment).
const CatalogDocMD = "# VGI hue worker\n\n" +
	"**An example [VGI](https://query.farm/vgi) worker that brings custom functions into DuckDB " +
	"SQL.** It was generated from the Query.Farm starter template and is meant to be renamed and " +
	"rewritten: every function here demonstrates one pattern you will reuse.\n\n" +
	"- `greet(name)` and `greet(name, style)`: a vectorized scalar with an overload and a " +
	"constrained constant argument.\n" +
	"- `sequence_table(count, step := 1)`: a deterministic table function that streams its " +
	"output in batches.\n" +
	"- `fetch_items(base_url, page_size := 100)`: wraps a paginated REST API, retrying politely " +
	"and reading a bearer token from a DuckDB secret or the environment.\n\n" +
	"The worker speaks Apache Arrow to DuckDB over stdin/stdout, a Unix socket, or HTTP, so the " +
	"same code runs as a local subprocess or a hosted service. See the " +
	"[VGI documentation](https://query.farm/vgi/docs/) to learn more."

const schemaDocLLM = "The `main` schema holds the starter template's example functions, one per common worker " +
	"pattern. `greet(name)` and `greet(name, style)` are vectorized scalars: use them inside a " +
	"projection or predicate, one result per row; `style` is a constant that must be 'casual' or " +
	"'formal'. `sequence_table(count, step := 1)` is a deterministic table function that returns " +
	"`count` rows of `(n, squared)`; use it for demos, tests, and as a template for generators. " +
	"`fetch_items(base_url, page_size := 100)` reads every item from a paginated JSON REST API as " +
	"`(id, name, category, price)` rows, streaming one page per batch, retrying rate-limited " +
	"requests, and authenticating with a `hue_api` DuckDB secret or the `HUE_API_TOKEN` " +
	"environment variable. Scalars cannot take named arguments; table functions can."

// SchemaDocMD is the main schema's human-facing description (also its comment).
const SchemaDocMD = "## main\n\n" +
	"The starter template's example functions, one per common worker pattern.\n\n" +
	"- **Scalars** answer one question per row and slot into a projection or predicate.\n" +
	"- **Table functions** return sets of rows, streamed to DuckDB in Arrow batches.\n" +
	"- **REST readers** page through an HTTP API and authenticate with a DuckDB secret."

// agentTask is one public analyst task for `vgi-lint simulate`.
type agentTask struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// agentTestTasks are graded privately by vgi-agent-tests.yaml.
var agentTestTasks = []agentTask{
	{Name: "formal_greeting", Prompt: "Using the hue worker, greet the name 'Grace' formally."},
	{Name: "sum_of_squares", Prompt: "Using the hue worker, what is the sum of the squared column for the first 10 rows " +
		"of the sequence with the default step?"},
	{Name: "stepped_sequence", Prompt: "List the n values of a 4-row sequence that steps by 5, in ascending order."},
	{Name: "items_per_category", Prompt: "The items API is running at https://vgi-starter-api.query-farm.services/v1. How many items does it have in each category?"},
}

// CatalogTags are the catalog-level discovery tags.
var CatalogTags = map[string]string{
	"vgi.title":              "VGI Hue Worker",
	"vgi.doc_llm":            catalogDocLLM,
	"vgi.doc_md":             CatalogDocMD,
	"vgi.source_url":         SourceURL,
	"vgi.icon_url":           "https://query.farm/media-kit/products/vgi/vgi-logo.png",
	"vgi.license":            "0BSD",
	"vgi.author":             "Query.Farm",
	"vgi.copyright":          "Copyright 2026 Query.Farm LLC",
	"vgi.keywords":           keywordsArray("vgi, duckdb, starter template, example worker, rest api, table function"),
	"vgi.support_contact":    SourceURL + "/issues",
	"vgi.support_policy_url": SourceURL + "#support",
	// Public analyst tasks for `vgi-lint simulate`; private graders live in vgi-agent-tests.yaml.
	"vgi.agent_test_tasks": mustJSON(agentTestTasks),
}

// schemaTags builds the main schema's tags; allExamples feeds vgi.example_queries.
func schemaTags(allExamples []exampleQuery) map[string]string {
	return map[string]string{
		"vgi.title":           "Hue Functions",
		"domain":              "examples",
		"vgi.keywords":        keywordsArray("greeting, sequence, generator, rest api, pagination"),
		"vgi.doc_llm":         schemaDocLLM,
		"vgi.doc_md":          SchemaDocMD,
		"vgi.categories":      mustJSON(SchemaCategories),
		"vgi.example_queries": mustJSON(allExamples),
	}
}
