package hue

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// testLights are a white-only lamp, a colour lamp with effects and a
// gradient strip, as the bridge's CLIP v2 JSON lists them.
const testLightsJSON = `[
 {"id":"l-white","owner":{"rid":"d-white","rtype":"device"},"metadata":{"name":"White lamp"},
  "on":{"on":true},"dimming":{"brightness":50}},
 {"id":"l-color","owner":{"rid":"d-color","rtype":"device"},"metadata":{"name":"Colour lamp"},
  "on":{"on":true},"dimming":{"brightness":50},"color_temperature":{"mirek":null},
  "color":{"xy":{"x":0.5,"y":0.4},"gamut_type":"C"},
  "effects":{"status":"no_effect","effect_values":["no_effect","candle","fire"]}},
 {"id":"l-strip","owner":{"rid":"d-strip","rtype":"device"},"metadata":{"name":"Strip"},
  "on":{"on":false},"dimming":{"brightness":70},"color_temperature":{"mirek":null},
  "color":{"xy":{"x":0.6915,"y":0.3083},"gamut_type":"C"},
  "gradient":{"points":[{"color":{"xy":{"x":0.7006,"y":0.2993}}},{"color":{"xy":{"x":0.1724,"y":0.7468}}}],
   "mode":"interpolated_palette","points_capable":5,"pixel_count":10,
   "mode_values":["interpolated_palette","interpolated_palette_mirrored","random_pixelated","segmented_palette"]}}
]`

func testRows(t *testing.T) map[string]row {
	t.Helper()
	var lights []hueLight
	if err := json.Unmarshal([]byte(testLightsJSON), &lights); err != nil {
		t.Fatal(err)
	}
	rows, err := lightRows(lights, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]row{}
	for _, r := range rows {
		out[r["light_id"].(string)] = r
	}
	return out
}

// updateBatch builds a one-row UPDATE input with the given LIGHTS columns.
func updateBatch(t *testing.T, vals map[string]any) arrow.RecordBatch {
	t.Helper()
	var fields []arrow.Field
	cols := columns{}
	for _, f := range LIGHTS.Schema.Fields() {
		if v, ok := vals[f.Name]; ok {
			fields = append(fields, f)
			cols[f.Name] = []any{v}
		}
	}
	b, err := buildBatch(arrow.NewSchema(fields, nil), cols, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Release)
	return b
}

// change runs rowChange + updateBody for one light and returns the JSON body.
func change(t *testing.T, light string, vals map[string]any) (string, row, error) {
	t.Helper()
	cur := testRows(t)[light]
	u, err := rowChange(updateBatch(t, vals), 0, cur)
	if err != nil {
		return "", nil, err
	}
	body, err := updateBody(u)
	if err != nil {
		return "", nil, err
	}
	b, _ := json.Marshal(body)
	return string(b), applyToRow(cur, body), nil
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want error containing %q, got %v", sub, err)
	}
}

func TestLightRowsGradientAndEffects(t *testing.T) {
	rows := testRows(t)
	s := rows["l-strip"]
	if s["gradient_mode"] != "interpolated_palette" || s["gradient_points_capable"] != int32(5) || s["gradient_pixel_count"] != int32(10) {
		t.Fatalf("strip gradient columns: %v", s)
	}
	hexes := s["gradient_hex"].([]any)
	if len(hexes) != 2 || hexes[0] != "#ff0000" || hexes[1] != "#00ff00" {
		t.Fatalf("gradient_hex = %v", hexes)
	}
	if xy := s["gradient_xy"].([]any)[1].(map[string]any); xy["x"] != 0.1724 || xy["y"] != 0.7468 {
		t.Fatalf("gradient_xy[1] = %v", xy)
	}
	if s["effect"] != nil || rows["l-color"]["effect"] != "no_effect" {
		t.Fatalf("effect: strip %v colour %v", s["effect"], rows["l-color"]["effect"])
	}
	w := rows["l-white"]
	for _, c := range []string{"gradient_hex", "gradient_xy", "gradient_mode", "gradient_points_capable", "effect", "color_x"} {
		if w[c] != nil {
			t.Fatalf("white lamp %s = %v, want NULL", c, w[c])
		}
	}
	// A colour on a gradient light empties its gradient.
	after := applyToRow(s, omap{{"color", xyBody(xyPoint{0.3, 0.3})}})
	if len(after["gradient_xy"].([]any)) != 0 || after["color_mode"] != "xy" {
		t.Fatalf("after colour %v", after)
	}
	// The rows build into the table schema (list of struct included).
	all := []row{rows["l-white"], rows["l-color"], rows["l-strip"]}
	b, err := buildBatch(lightsTableSchema, rowsToColumns(all, LIGHTS.Names), len(all))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	if _, err := writeResultBatch("rows", all, all); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateXY(t *testing.T) {
	body, nr, err := change(t, "l-color", map[string]any{"color_x": 0.3, "color_y": 0.6})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"color":{"xy":{"x":0.3,"y":0.6}}}` {
		t.Fatalf("body %s", body)
	}
	if nr["color_mode"] != "xy" || nr["color_x"] != 0.3 || nr["color_y"] != 0.6 {
		t.Fatalf("row %v", nr)
	}
	// Only one coordinate: the other is the light's current one.
	body, _, err = change(t, "l-color", map[string]any{"color_y": 0.35})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"color":{"xy":{"x":0.5,"y":0.35}}}` {
		t.Fatalf("body %s", body)
	}
	_, _, err = change(t, "l-color", map[string]any{"color_x": 0.3, "color_hex": "#ff0000"})
	wantErr(t, err, "set either color_hex or color_x/color_y, not both")
	_, _, err = change(t, "l-color", map[string]any{"color_x": 1.5})
	wantErr(t, err, "color_x must be 0–1, got 1.5")
	_, _, err = change(t, "l-color", map[string]any{"color_y": -0.1})
	wantErr(t, err, "color_y must be 0–1, got -0.1")
	_, _, err = change(t, "l-white", map[string]any{"color_x": 0.3, "color_y": 0.3})
	wantErr(t, err, "light 'White lamp' does not support color")
	_, _, err = change(t, "l-white", map[string]any{"gradient_mode": "segmented_palette"})
	wantErr(t, err, "light 'White lamp' does not support gradients")
	_, _, err = change(t, "l-color", map[string]any{"color_x": 0.3, "color_temperature_kelvin": int32(3000)})
	wantErr(t, err, "either kelvin or color")
}

func TestUpdateGradient(t *testing.T) {
	body, nr, err := change(t, "l-strip", map[string]any{"gradient_hex": []any{"#0000ff", "#ff0000", "#00ff00"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"gradient":{"points":[{"color":{"xy":{"x":0.1355,"y":0.0399}}},{"color":{"xy":{"x":0.7006,"y":0.2993}}},` +
		`{"color":{"xy":{"x":0.1724,"y":0.7468}}}]}}`
	if body != want {
		t.Fatalf("body %s\nwant %s", body, want)
	}
	if got := nr["gradient_hex"].([]any); len(got) != 3 || got[0] != "#0000ff" || got[1] != "#ff0000" || got[2] != "#00ff00" {
		t.Fatalf("gradient_hex after %v", got)
	}
	body, nr, err = change(t, "l-strip", map[string]any{
		"gradient_xy":   []any{map[string]any{"x": 0.2, "y": 0.3}, map[string]any{"x": 0.4, "y": 0.5}},
		"gradient_mode": "segmented_palette"})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"gradient":{"points":[{"color":{"xy":{"x":0.2,"y":0.3}}},{"color":{"xy":{"x":0.4,"y":0.5}}}],"mode":"segmented_palette"}}` {
		t.Fatalf("body %s", body)
	}
	if nr["gradient_mode"] != "segmented_palette" || len(nr["gradient_xy"].([]any)) != 2 {
		t.Fatalf("row %v", nr)
	}
	// The bridge sets the colour to the first gradient point; RETURNING shows it.
	if nr["color_mode"] != "xy" || nr["color_x"] != 0.2 || nr["color_y"] != 0.3 || nr["color_hex"] != xyToHex(0.2, 0.3) ||
		nr["color_temperature_kelvin"] != nil {
		t.Fatalf("colour after gradient %v", nr)
	}
	// A mode change alone re-sends the current points.
	body, _, err = change(t, "l-strip", map[string]any{"gradient_mode": "random_pixelated"})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"gradient":{"points":[{"color":{"xy":{"x":0.7006,"y":0.2993}}},{"color":{"xy":{"x":0.1724,"y":0.7468}}}],"mode":"random_pixelated"}}` {
		t.Fatalf("body %s", body)
	}
	six := []any{"#ff0000", "#ff0000", "#ff0000", "#ff0000", "#ff0000", "#ff0000"}
	_, _, err = change(t, "l-strip", map[string]any{"gradient_hex": six})
	wantErr(t, err, "light 'Strip' takes 2–5 gradient points, got 6")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_hex": []any{"#ff0000"}})
	wantErr(t, err, "light 'Strip' takes 2–5 gradient points, got 1")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_xy": []any{map[string]any{"x": 0.2, "y": 0.3}}})
	wantErr(t, err, "light 'Strip' takes 2–5 gradient points, got 1")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_hex": []any{}})
	wantErr(t, err, "takes 2–5 gradient points, got 0")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_mode": "sparkly"})
	wantErr(t, err, "invalid gradient_mode 'sparkly' for light 'Strip'; allowed: interpolated_palette, interpolated_palette_mirrored, random_pixelated, segmented_palette")
	_, _, err = change(t, "l-color", map[string]any{"gradient_hex": []any{"#ff0000"}})
	wantErr(t, err, "light 'Colour lamp' does not support gradients")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_hex": []any{"#ff0000", nil}})
	wantErr(t, err, "gradient points must not be NULL")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_xy": []any{map[string]any{"x": 0.2, "y": nil}}})
	wantErr(t, err, "gradient points must not be NULL")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_xy": []any{map[string]any{"x": 1.2, "y": 0.1}}})
	wantErr(t, err, "gradient x and y must be 0–1, got (1.2, 0.1)")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_hex": []any{"#ff0000"}, "gradient_xy": []any{map[string]any{"x": 0.2, "y": 0.1}}})
	wantErr(t, err, "set either gradient_hex or gradient_xy, not both")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_hex": []any{"#ff0000"}, "color_hex": "#00ff00"})
	wantErr(t, err, "not both")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_mode": "segmented_palette", "color_x": 0.3})
	wantErr(t, err, "set either a gradient or a single colour (color_hex, color_x/color_y, color_temperature_kelvin), not both")
	_, _, err = change(t, "l-strip", map[string]any{"gradient_mode": "segmented_palette", "color_temperature_kelvin": int32(3000)})
	wantErr(t, err, "not both")
	// Unchanged columns (as RETURNING sends them) do not count as SET.
	cur := testRows(t)["l-strip"]
	body, _, err = change(t, "l-strip", map[string]any{"gradient_mode": "segmented_palette", "color_x": cur["color_x"],
		"gradient_xy": cur["gradient_xy"], "gradient_hex": cur["gradient_hex"]})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"mode":"segmented_palette"`) {
		t.Fatalf("body %s", body)
	}
}

// A mode change alone on a light whose gradient a single colour replaced has
// no points to re-send: rowChange refuses it, so no request is built or sent.
func TestUpdateGradientModeWithoutGradient(t *testing.T) {
	_, cleared, err := change(t, "l-strip", map[string]any{"color_hex": "#00ff00"})
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := cleared["gradient_xy"].([]any); len(g) != 0 {
		t.Fatalf("gradient_xy after a single colour = %v", cleared["gradient_xy"])
	}
	const msg = "light 'Strip' has no gradient to change the mode of; set gradient_hex or gradient_xy too"
	_, err = rowChange(updateBatch(t, map[string]any{"gradient_mode": "segmented_palette"}), 0, cleared)
	if err == nil || err.Error() != msg {
		t.Fatalf("want %q, got %v", msg, err)
	}
	one := testRows(t)["l-strip"]
	one["gradient_xy"] = []any{map[string]any{"x": 0.2, "y": 0.3}}
	_, err = rowChange(updateBatch(t, map[string]any{"gradient_mode": "segmented_palette"}), 0, one)
	if err == nil || err.Error() != msg {
		t.Fatalf("one point: want %q, got %v", msg, err)
	}
	// Setting the points in the same statement is fine.
	_, _, err = change(t, "l-strip", map[string]any{"gradient_mode": "segmented_palette",
		"gradient_hex": []any{"#ff0000", "#0000ff"}})
	if err != nil {
		t.Fatal(err)
	}
	// A non-gradient light still says so.
	_, _, err = change(t, "l-color", map[string]any{"gradient_mode": "segmented_palette"})
	wantErr(t, err, "light 'Colour lamp' does not support gradients")
}

// set_gradient refuses an empty or NULL colour list at bind time.
func TestSetGradientArgsNeedsTwoColours(t *testing.T) {
	mem := memory.NewGoAllocator()
	name := array.NewStringBuilder(mem)
	name.Append("Strip")
	light := name.NewArray()
	defer light.Release()
	for _, null := range []bool{false, true} {
		lb := array.NewListBuilder(mem, arrow.BinaryTypes.String)
		if null {
			lb.AppendNull()
		} else {
			lb.Append(true)
		}
		colors := lb.NewArray()
		args := &vgi.Arguments{Positional: []arrow.Array{light, colors}, Named: map[string]arrow.Array{}}
		_, _, _, err := gradientArgs(args)
		if err == nil || err.Error() != "set_gradient needs at least two colours" {
			t.Fatalf("null=%v: got %v", null, err)
		}
		colors.Release()
		lb.Release()
	}
}

func TestUpdateEffect(t *testing.T) {
	body, nr, err := change(t, "l-color", map[string]any{"effect": "candle"})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"effects":{"effect":"candle"}}` || nr["effect"] != "candle" {
		t.Fatalf("body %s row %v", body, nr["effect"])
	}
	_, _, err = change(t, "l-color", map[string]any{"effect": "disco"})
	wantErr(t, err, "invalid effect 'disco' for light 'Colour lamp'; allowed: no_effect, candle, fire")
	_, _, err = change(t, "l-white", map[string]any{"effect": "candle"})
	wantErr(t, err, "light 'White lamp' does not support effects")
}

func TestCoalesceColourAndGradient(t *testing.T) {
	g := omap{{"gradient", gradientBody([]xyPoint{{0.1, 0.2}}, "")}}
	c := omap{{"color", xyBody(xyPoint{0.3, 0.3})}}
	out := coalesce([]stagedRequest{{Kind: "light", Target: "a", Body: c}, {Kind: "light", Target: "a", Body: g}})
	if len(out) != 1 || out[0].Body.has("color") || !out[0].Body.has("gradient") {
		t.Fatalf("coalesced %v", out)
	}
	out = coalesce([]stagedRequest{{Kind: "light", Target: "a", Body: g}, {Kind: "light", Target: "a", Body: c}})
	if len(out) != 1 || out[0].Body.has("gradient") || !out[0].Body.has("color") {
		t.Fatalf("coalesced %v", out)
	}
}

func TestGradientBodyRoundTripsThroughStaging(t *testing.T) {
	body := omap{{"gradient", gradientBody([]xyPoint{{0.1, 0.2}, {0.3, 0.4}}, "segmented_palette")}}
	b, _ := json.Marshal(body)
	var back omap
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	r := applyToRow(row{}, back)
	if r["gradient_mode"] != "segmented_palette" || len(r["gradient_xy"].([]any)) != 2 {
		t.Fatalf("row %v", r)
	}
}
