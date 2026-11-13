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

const catalogDocLLM = "Read and control Philips Hue smart lighting from SQL through a Hue bridge on the local network " +
	"(CLIP v2 API). Connect by passing the application key as the ATTACH option `app_key`; the bridge " +
	"is found automatically on the local network, or set the `bridge` option to its address or bridge " +
	"id (the worker's HUE_APP_KEY and HUE_BRIDGE environment variables are fallbacks). Concepts: " +
	"every light belongs to a physical device; devices are placed in a room; a zone is any grouping " +
	"of individual lights; a scene is a saved lighting state for one room or zone; a sensor is a " +
	"service on a device (a Hue motion sensor also reports temperature, ambient light and battery). " +
	"Inventory is exposed as tables that always read live bridge state; the lights table also accepts " +
	"UPDATE (power, brightness, white temperature, colour as hex or CIE xy, gradient points and " +
	"mode on gradient lights, effects, and name). Changes can also be made with table functions " +
	"that send a command when the query runs and return one row per change, matching targets by " +
	"id or case-insensitive name; brightness is a percentage and white temperature is in Kelvin. " +
	"They switch real lights in someone's home, so call them only when the user asks. With the ATTACH " +
	"option `transactional true`, changes inside BEGIN ... COMMIT are staged and sent together at " +
	"COMMIT (ROLLBACK discards them)."

// CatalogDocMD is the catalog's long human-facing description (also its comment).
const CatalogDocMD = "# Philips Hue for DuckDB\n\n**Query and control your Hue lights with SQL.** This [VGI](https://query.farm/vgi) " +
	"worker talks to a Hue bridge on your network: read the live state of every light, room, zone, " +
	"scene and sensor as tables, join it with anything else in DuckDB, and switch lights from a " +
	"query.\n\nAttach it with an application key (created by pressing the bridge's link button); " +
	"the bridge itself is discovered on your network. Pass the key with `getenv(...)` to keep it " +
	"out of your SQL; DuckDB redacts it either way."

const schemaDocLLM = "Everything for the attached Hue bridge lives in `main`. The inventory tables take no arguments " +
	"and reflect the bridge at query time; filter and join them like any table. The lights table " +
	"also accepts UPDATE of `is_on`, `brightness`, `color_temperature_kelvin`, `color_hex`, `color_x`/`color_y`, " +
	"`gradient_hex`/`gradient_xy`, `gradient_mode`, `effect` and `name`; `set_gradient(light, colors, " +
	"mode := ...)` sets a gradient light's points. The control functions take a target (id or case-insensitive " +
	"name) plus named, optional state arguments, at least one of which is required: `power := true|false` " +
	"(on/off; `on` is a SQL keyword), `brightness := 0..100` (above 0 implies on, 0 turns off), " +
	"`kelvin := 2000..6500` or `color := '#rrggbb'` (one or the other) and `transition_ms` (fade " +
	"time). Each returns rows of `target_type`, `target_id`, `name`, `ok`, `error`, `request`, `settled`. " +
	"For many lights at once, feed a query that has a `light_id` column into the bulk function."

// SchemaDocMD is the main schema's human-facing description (also its comment).
const SchemaDocMD = "## main\n\nEvery table and function for the attached Hue bridge.\n\n- **Inventory tables** " +
	"show what exists and its live state.\n- **Control functions** change a light, a room or zone, " +
	"or recall a scene, and return what they sent."

// agentTask is one public analyst task for `vgi-lint simulate`.
type agentTask struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// agentTestTasks are graded privately by vgi-agent-tests.yaml.
var agentTestTasks = []agentTask{
	{Name: "lights_on_per_room", Prompt: "Using the hue worker, how many lights are on in each room?"},
	{Name: "active_scenes", Prompt: "Using the hue worker, which scene is currently active in each room?"},
	{Name: "biggest_room", Prompt: "Using the hue worker, which room has the most lights?"},
	{Name: "warmest_room", Prompt: "Using the hue worker, which room has the highest temperature sensor reading?"},
	{Name: "kitchen_off", Prompt: "Using the hue worker, turn off all the lights in the Kitchen."},
	{Name: "dim_sofa", Prompt: "Using the hue worker, set the Sofa lamp to 30% brightness."},
	{Name: "rename_desk_lamp", Prompt: "Using the hue worker, rename the light called Desk lamp to Reading lamp."},
	{Name: "relax_living_room", Prompt: "Using the hue worker, activate the Relax scene in the Living room."},
	{Name: "strip_gradient", Prompt: "Using the hue worker, make the Hallway strip a gradient from red to blue."},
	{Name: "warm_white_everywhere", Prompt: "Using the hue worker, set every light that supports colour temperature to 2700 K."},
}

// CatalogTags are the catalog-level discovery tags.
var CatalogTags = map[string]string{
	"vgi.title":              "VGI Hue Worker",
	"vgi.subtitle":           "Read and control Philips Hue lights, rooms, scenes and sensors from SQL.",
	"vgi.doc_llm":            catalogDocLLM,
	"vgi.doc_md":             CatalogDocMD,
	"vgi.source_url":         SourceURL,
	"vgi.icon_url":           "https://query.farm/media-kit/products/vgi/vgi-logo.png",
	"vgi.license":            "0BSD",
	"vgi.author":             "Query.Farm",
	"vgi.copyright":          "Copyright 2026 Query.Farm LLC",
	"vgi.keywords":           keywordsArray("philips hue, smart home, lights, home automation, sensors, duckdb, vgi"),
	"vgi.support_contact":    SourceURL + "/issues",
	"vgi.support_policy_url": SourceURL + "#support",
	// Public analyst tasks for `vgi-lint simulate`; private graders live in vgi-agent-tests.yaml.
	"vgi.agent_test_tasks": mustJSON(agentTestTasks),
}

// schemaTags builds the main schema's tags; allExamples feeds vgi.example_queries.
func schemaTags(allExamples []exampleQuery) map[string]string {
	return map[string]string{
		"vgi.title":           "Hue Functions",
		"domain":              "smart home",
		"vgi.keywords":        keywordsArray("hue, lights, rooms, scenes, sensors, light control"),
		"vgi.doc_llm":         schemaDocLLM,
		"vgi.doc_md":          SchemaDocMD,
		"vgi.categories":      mustJSON(SchemaCategories),
		"vgi.example_queries": mustJSON(allExamples),
	}
}
