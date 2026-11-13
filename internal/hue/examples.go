package hue

import (
	"encoding/json"

	"github.com/Query-farm/vgi-go/vgi"
)

// functionExamples are each control function's example queries, published as
// its FunctionInfo examples and its vgi.example_queries tag, and collected
// into the schema-level vgi.example_queries list (allExamples).
var functionExamples = map[string][]exampleQuery{
	"set_light": {
		{Description: "Dim the desk lamp to 40% warm white",
			SQL: "SELECT name, ok FROM hue.main.set_light('Desk lamp', brightness := 40, kelvin := 2700)"},
		{Description: "Turn the desk lamp off",
			SQL: "SELECT name, ok FROM hue.main.set_light('Desk lamp', power := false)"},
	},
	"set_gradient": {
		{Description: "A red-orange-blue gradient on the hallway lightstrip",
			SQL: "SELECT name, ok FROM hue.main.set_gradient('Hallway strip', ['#ff0000', '#ff8000', '#0000ff'])"},
		{Description: "Two solid segments, green and blue",
			SQL: "SELECT name, request FROM hue.main.set_gradient('Hallway strip', ['#00ff00', '#0000ff'], mode := 'segmented_palette')"},
	},
	"set_room": {
		{Description: "Turn off every light in the kitchen",
			SQL: "SELECT name, ok FROM hue.main.set_room('Kitchen', power := false)"},
		{Description: "Fade the living room to dim orange over two seconds",
			SQL: "SELECT name, ok FROM hue.main.set_room('Living room', brightness := 30, color := 'orange', transition_ms := 2000)"},
	},
	"recall_scene": {
		{Description: "Activate the living room's Relax scene",
			SQL: "SELECT name, ok FROM hue.main.recall_scene('Relax', room := 'Living room')"},
	},
	"set_lights": {
		{Description: "Turn off every light that is on, except in the bedroom",
			SQL: "SELECT name, ok, error FROM hue.main.set_lights((SELECT light_id, name FROM hue.main.lights " +
				"WHERE is_on AND room <> 'Bedroom'), power := false)"},
		{Description: "Fade every colour-capable light to warm white",
			SQL: "SELECT count(*) FILTER (WHERE ok) AS changed FROM hue.main.set_lights((SELECT light_id FROM hue.main.lights " +
				"WHERE color_mode IS NOT NULL), kelvin := 2700, transition_ms := 1000)"},
	},
}

// functionOrder fixes the order of function examples in the schema-level list.
var functionOrder = []string{"set_light", "set_room", "recall_scene", "set_lights", "set_gradient"}

// withExamples attaches name's examples to md, as FunctionInfo examples and
// as the vgi.example_queries tag.
func withExamples(name string, md vgi.FunctionMetadata) vgi.FunctionMetadata {
	exs := functionExamples[name]
	if len(exs) == 0 {
		panic("no examples for " + name)
	}
	tags := make(map[string]string, len(md.Tags)+1)
	for k, v := range md.Tags {
		tags[k] = v
	}
	tags["vgi.example_queries"] = mustJSON(exs)
	md.Tags = tags
	for _, e := range exs {
		md.Examples = append(md.Examples, vgi.CatalogExample{SQL: e.SQL, Description: e.Description})
	}
	return md
}

// allExamples is every table's and function's example, for the schema's vgi.example_queries.
func allExamples() []exampleQuery {
	var out []exampleQuery
	for _, inv := range inventories {
		var exs []exampleQuery
		if err := json.Unmarshal([]byte(inv.tags["vgi.example_queries"]), &exs); err != nil {
			panic(err)
		}
		out = append(out, exs...)
	}
	for _, name := range functionOrder {
		out = append(out, functionExamples[name]...)
	}
	return out
}

// executableExample is one entry of vgi.executable_examples: vgi-lint runs it
// against the attached worker and compares its rows with ExpectedResult.
type executableExample struct {
	Description    string              `json:"description"`
	SQL            string              `json:"sql"`
	ExpectedResult []map[string]string `json:"expected_result"`
}

func init() {
	lightsInv.tags["vgi.executable_examples"] = mustJSON([]executableExample{{
		Description: "The state columns of the lights table (reads only its schema, so no bridge is needed)",
		SQL: "SELECT column_name, column_type FROM (DESCRIBE hue.main.lights) " +
			"WHERE column_name IN ('light_id', 'is_on', 'brightness', 'color_temperature_kelvin', 'gradient_xy') ORDER BY column_name",
		ExpectedResult: []map[string]string{
			{"column_name": "brightness", "column_type": "DOUBLE"},
			{"column_name": "color_temperature_kelvin", "column_type": "INTEGER"},
			{"column_name": "gradient_xy", "column_type": "STRUCT(x DOUBLE, y DOUBLE)[]"},
			{"column_name": "is_on", "column_type": "BOOLEAN"},
			{"column_name": "light_id", "column_type": "VARCHAR"},
		},
	}})
}
