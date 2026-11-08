// Scalar function example: greet.
//
// A scalar maps each input row to one output row. VGI hands ProcessTyped a
// whole Arrow column at a time (not one value), so build the output column in
// one pass.
//
// SQL has no named arguments for scalar functions, so an optional argument is
// an *overload*: two functions that share the SQL name greet and differ in
// arity. DuckDB picks one by the number of arguments.
//
// style is a constant argument (the default for fields without const=false).
// It must be a constant in the query, not a column, and its choices are
// enforced when the query is bound, so greet('Ada', 'loud') fails with a
// helpful error before any row runs.

package starter

import (
	"context"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// Styles are the accepted values of greet's style argument.
var Styles = []string{"casual", "formal"}

var (
	greetPrefix = map[string]string{"casual": "Hi, ", "formal": "Good day, "}
	greetSuffix = map[string]string{"casual": "!", "formal": "."}
)

// Greeting returns the greeting for one name in the given style.
func Greeting(name, style string) string {
	return greetPrefix[style] + name + greetSuffix[style]
}

// greetColumn builds greetings for a whole column; NULL names stay NULL.
func greetColumn(params *vgi.ProcessParams, batch arrow.RecordBatch, style string) (arrow.RecordBatch, error) {
	return vgi.MapColumn(params, batch, 0, array.NewStringBuilder,
		func(col arrow.Array, i int) string { return Greeting(vgi.GetStringValue(col, i), style) })
}

// greetArgs declares greet(name). `vgi:"..."` tags drive both the advertised
// signature and runtime binding. doc= must be the last key in a tag.
type greetArgs struct {
	Name *array.String `vgi:"pos=0,const=false,doc=Person or thing to greet; NULL in, NULL out."`
}

// greetWithStyleArgs declares greet(name, style).
type greetWithStyleArgs struct {
	Name  *array.String `vgi:"pos=0,const=false,doc=Person or thing to greet; NULL in, NULL out."`
	Style string        `vgi:"pos=1,choices='casual,formal',doc=Greeting tone: 'casual' gives 'Hi, <name>!' and 'formal' gives 'Good day, <name>.' (a constant)"`
}

// Examples are package-level so overloads can publish each other's
// (vgi.example_queries is per SQL name) and the schema can list them all.
var (
	greetExamples = []vgi.CatalogExample{
		{SQL: "SELECT starter.main.greet('Ada') AS greeting", Description: "Greet one name casually"},
	}
	greetWithStyleExamples = []vgi.CatalogExample{
		{SQL: "SELECT starter.main.greet('Ada', 'formal') AS greeting", Description: "Greet one name formally"},
	}
)

// Greet is greet(name): a casual greeting for every row.
type Greet struct{}

// Name is the SQL name.
func (*Greet) Name() string { return "greet" }

// Metadata describes the function to DuckDB, agents and vgi-lint.
func (*Greet) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description:  "Greet a name casually: greet('Ada') returns 'Hi, Ada!'",
		Stability:    vgi.StabilityConsistent,
		NullHandling: vgi.NullHandlingDefault,
		ReturnType:   arrow.BinaryTypes.String,
		Tags: withExamples(objectTags(docs{
			Title:    "Greet (casual)",
			Category: "text",
			Keywords: "greet, greeting, hello, salutation, welcome message",
			DocLLM: "Return a **casual greeting** for each input name: greet('Ada') returns " +
				"'Hi, Ada!'. NULL names produce NULL. This is the one-argument overload; " +
				"call greet(name, style) for the formal tone. It is a vectorized scalar, " +
				"so it works over a whole column of names.",
			DocMD: "## greet(name)\n\n" +
				"A casual greeting for each row: greet('Ada') returns 'Hi, Ada!'.\n\n" +
				"NULL in, NULL out. See greet(name, style) for the formal tone.\n\n" +
				"```sql\nSELECT starter.main.greet(name) FROM people;\n```",
		}), greetExamples, greetWithStyleExamples),
		Examples: greetExamples,
	}
}

// OnBindTyped declares the return type.
func (*Greet) OnBindTyped(_ *greetArgs, _ *vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindResult(arrow.BinaryTypes.String)
}

// ProcessTyped returns "Hi, <name>!" for every row.
func (*Greet) ProcessTyped(_ context.Context, _ *greetArgs, params *vgi.ProcessParams, batch arrow.RecordBatch) (arrow.RecordBatch, error) {
	return greetColumn(params, batch, "casual")
}

// GreetWithStyle is greet(name, style): a greeting in the chosen tone.
type GreetWithStyle struct{}

// Name is the SQL name (shared with Greet: an overload).
func (*GreetWithStyle) Name() string { return "greet" }

// Metadata describes the function to DuckDB, agents and vgi-lint.
func (*GreetWithStyle) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description:  "Greet a name in a chosen tone: greet('Ada', 'formal') returns 'Good day, Ada.'",
		Stability:    vgi.StabilityConsistent,
		NullHandling: vgi.NullHandlingDefault,
		ReturnType:   arrow.BinaryTypes.String,
		Tags: withExamples(objectTags(docs{
			Title:    "Greet (with tone)",
			Category: "text",
			Keywords: "greet, greeting, formal greeting, casual greeting, salutation, tone",
			DocLLM: "Return a greeting for each input name in the requested tone. style must be a " +
				"constant, either 'casual' ('Hi, Ada!') or 'formal' ('Good day, Ada.'); " +
				"any other value is rejected when the query is planned. NULL names produce NULL.",
			DocMD: "## greet(name, style)\n\n" +
				"A greeting in the chosen tone: 'casual' or 'formal'.\n\n" +
				"```sql\nSELECT starter.main.greet('Ada', 'formal');  -- Good day, Ada.\n```",
		}), greetExamples, greetWithStyleExamples),
		Examples: greetWithStyleExamples,
	}
}

// OnBindTyped declares the return type; style's choices were already checked.
func (*GreetWithStyle) OnBindTyped(_ *greetWithStyleArgs, _ *vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindResult(arrow.BinaryTypes.String)
}

// ProcessTyped returns a greeting in args.Style for every row.
func (*GreetWithStyle) ProcessTyped(_ context.Context, args *greetWithStyleArgs, params *vgi.ProcessParams, batch arrow.RecordBatch) (arrow.RecordBatch, error) {
	return greetColumn(params, batch, args.Style)
}
