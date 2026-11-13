// UPDATE hue.main.lights SET ...: port of hue-worker/src/lights_write.ts.
// lights_update is the lights CatalogTable's UpdateFunction; write_options and
// the count/rows/changes result shapes use the SDK's write-result helpers.

package hue

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
)

var updatableLightColumns = []string{"is_on", "brightness", "color_temperature_kelvin", "color_x", "color_y", "color_hex",
	"gradient_hex", "gradient_xy", "gradient_mode", "effect", "name"}

// writeResultBatch is vgi.WriteResultBatch over the port's row maps.
func writeResultBatch(mode vgi.WriteResultMode, oldRows, newRows []row) (arrow.RecordBatch, error) {
	conv := func(rs []row) []map[string]any {
		out := make([]map[string]any, len(rs))
		for i, r := range rs {
			out[i] = r
		}
		return out
	}
	return vgi.WriteResultBatch(mode, LIGHTS.Schema, conv(oldRows), conv(newRows))
}

// --------------------------------------------------------------- validation

func checkKnownColumns(names []string) error {
	var unknown []string
	for _, c := range names {
		if c != "rowid" && !slices.Contains(LIGHTS.Names, c) {
			unknown = append(unknown, "'"+c+"'")
		}
	}
	if len(unknown) > 0 {
		return hueErr("hue.main.lights has no column %s", strings.Join(unknown, ", "))
	}
	return nil
}

// lightUpdate is what one UPDATE row asks of one light.
type lightUpdate struct {
	state lightChange // on, brightness, kelvin, color_hex
	name  *string
	xy    *xyPoint // color_x/color_y, completed from the current colour
	grad  omap     // the gradient object to send, or nil
	eff   *string
}

// lightName is the row's light name for error messages.
func lightName(r row) string {
	if n, ok := r["name"].(string); ok {
		return n
	}
	id, _ := r["light_id"].(string)
	return id
}

// xyList reads a gradient_xy value ([{x, y}, ...]).
func xyList(v any) ([]xyPoint, error) {
	items, _ := v.([]any)
	out := make([]xyPoint, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, errNullPoint
		}
		x, okx := num(m["x"])
		y, oky := num(m["y"])
		if !okx || !oky {
			return nil, errNullPoint
		}
		if !in01(x) || !in01(y) {
			return nil, hueErr("gradient x and y must be 0–1, got (%s, %s)", jsNum(x), jsNum(y))
		}
		out = append(out, xyPoint{x, y})
	}
	return out, nil
}

// rowChange is what input row i asks to change relative to the current row.
// A column counts as SET only when its value differs from the current row
// (with RETURNING DuckDB sends every column). Checks run in the order of
// COLOR-SPEC: read-only columns, NULLs, conflicting columns, capabilities,
// values, then the name.
func rowChange(batch arrow.RecordBatch, i int, current row) (lightUpdate, error) {
	var u lightUpdate
	var refused []string
	set := map[string]any{}
	for fi, f := range batch.Schema().Fields() {
		if f.Name == "rowid" {
			continue
		}
		v := cellValue(batch.Column(fi), i)
		if same(v, current[f.Name]) {
			continue
		}
		if !slices.Contains(updatableLightColumns, f.Name) {
			refused = append(refused, "'"+f.Name+"'")
			continue
		}
		set[f.Name] = v
	}
	if len(refused) > 0 {
		return u, hueErr("cannot UPDATE %s of hue.main.lights; updatable columns: %s",
			strings.Join(refused, ", "), strings.Join(updatableLightColumns, ", "))
	}
	for _, c := range updatableLightColumns {
		if v, ok := set[c]; ok && v == nil {
			return u, hueErr("cannot SET %s = NULL on a light", c)
		}
	}
	has := func(cols ...string) bool {
		for _, c := range cols {
			if _, ok := set[c]; ok {
				return true
			}
		}
		return false
	}
	wantXY := has("color_x", "color_y")
	wantGrad := has("gradient_hex", "gradient_xy", "gradient_mode")
	switch {
	case has("color_hex") && wantXY:
		return u, hueErr("set either color_hex or color_x/color_y, not both")
	case has("gradient_hex") && has("gradient_xy"):
		return u, hueErr("set either gradient_hex or gradient_xy, not both")
	case wantGrad && has("color_hex", "color_x", "color_y", "color_temperature_kelvin"):
		return u, hueErr("set either a gradient or a single colour (color_hex, color_x/color_y, color_temperature_kelvin), not both")
	}
	name := lightName(current)
	g, _ := current[capGradient].(*hueGradient)
	effects, _ := current[capEffects].([]string)
	if capable, _ := current[capColor].(bool); wantXY && !capable {
		return u, hueErr("light '%s' does not support color", name)
	}
	if wantGrad && g == nil {
		return u, hueErr("light '%s' does not support gradients", name)
	}
	if has("effect") && effects == nil {
		return u, hueErr("light '%s' does not support effects", name)
	}
	for c, v := range set {
		switch c {
		case "is_on":
			b, _ := v.(bool)
			u.state.On = &b
		case "brightness":
			n, _ := num(v)
			u.state.Brightness = &n
		case "color_temperature_kelvin":
			n, _ := num(v)
			u.state.Kelvin = &n
		case "color_hex":
			s := fmt.Sprint(v)
			u.state.Color = &s
		case "effect":
			s := fmt.Sprint(v)
			u.eff = &s
		case "name":
			s := fmt.Sprint(v)
			u.name = &s
		}
	}
	if wantXY {
		cx, _ := num(current["color_x"])
		cy, _ := num(current["color_y"])
		p := xyPoint{cx, cy}
		for _, c := range []string{"color_x", "color_y"} {
			v, ok := set[c]
			if !ok {
				continue
			}
			n, _ := num(v)
			if !in01(n) {
				return u, hueErr("%s must be 0–1, got %s", c, jsNum(n))
			}
			if c == "color_x" {
				p.X = n
			} else {
				p.Y = n
			}
		}
		u.xy = &p
	}
	if wantGrad {
		var points []xyPoint
		var err error
		switch {
		case has("gradient_hex"):
			items, _ := set["gradient_hex"].([]any)
			points, err = hexesToXY(items)
		case has("gradient_xy"):
			points, err = xyList(set["gradient_xy"])
		default:
			// A mode change alone re-sends the light's current points with it
			// (CLIP v2 requires the points in every gradient write).
			points, err = xyList(current["gradient_xy"])
			// No current gradient (e.g. a single colour replaced it): there
			// are no points to re-send, so refuse rather than send none.
			if err == nil && g != nil && len(points) < 2 {
				return u, hueErr("light '%s' has no gradient to change the mode of; set gradient_hex or gradient_xy too", name)
			}
		}
		if err != nil {
			return u, err
		}
		mode := ""
		if v, ok := set["gradient_mode"]; ok {
			mode = fmt.Sprint(v)
		}
		if err := checkGradient(name, g, points, mode); err != nil {
			return u, err
		}
		u.grad = gradientBody(points, mode)
	}
	if u.eff != nil {
		if err := checkEffect(name, effects, *u.eff); err != nil {
			return u, err
		}
	}
	if u.name != nil && (strings.TrimSpace(*u.name) == "" || len([]rune(*u.name)) > 32) {
		return u, hueErr("light names must be 1-32 characters, got '%s'", *u.name)
	}
	if u.xy != nil && u.state.Kelvin != nil {
		return u, hueErr("pass either kelvin or color, not both")
	}
	return u, nil
}

func updateBody(u lightUpdate) (omap, error) {
	body := omap{}
	ch := u.state
	if ch.On != nil || ch.Brightness != nil || ch.Kelvin != nil || ch.Color != nil {
		var err error
		if body, err = buildStateBody(ch); err != nil {
			return nil, err
		}
	}
	if u.xy != nil {
		body = body.set("color", xyBody(*u.xy))
	}
	if u.grad != nil {
		body = body.set("gradient", u.grad)
	}
	if u.eff != nil {
		body = body.set("effects", omap{{"effect", *u.eff}})
	}
	if u.name != nil {
		body = body.set("metadata", omap{{"name", *u.name}})
	}
	return body, nil
}

// updateRequests splits a rename into its own request (a bridge ignores state sent with one).
func updateRequests(body omap) []omap {
	var out []omap
	if state := body.without("metadata"); len(state) > 0 {
		out = append(out, state)
	}
	if md, ok := body.get("metadata"); ok {
		out = append(out, omap{{"metadata", md}})
	}
	return out
}

// ------------------------------------------------------------- the function

// NoState is the empty per-call state of a table in-out function.
type NoState struct{}

type lightsUpdateFn struct{}

var _ vgi.TypedTableInOutFunc[NoState] = lightsUpdateFn{}

func (lightsUpdateFn) Name() string { return "lights_update" }

func (lightsUpdateFn) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{Description: "Backs UPDATE hue.main.lights: apply SET values to each matched light",
		Stability: vgi.StabilityVolatile}
}

func (lightsUpdateFn) ArgumentSpecs() []vgi.ArgSpec {
	return []vgi.ArgSpec{
		{Name: "data", Position: 0, ArrowType: "table", Doc: "The changed columns plus rowid of each light to update; supplied by DuckDB."},
		vgi.WriteOptionsArgSpec(),
	}
}

func (lightsUpdateFn) OnBind(p *vgi.BindParams) (*vgi.BindResponse, error) {
	var names []string
	if p.InputSchema != nil {
		names = make([]string, p.InputSchema.NumFields())
		for i, f := range p.InputSchema.Fields() {
			names[i] = f.Name
		}
		if err := checkKnownColumns(names); err != nil {
			return nil, err
		}
	}
	opts, err := vgi.ParseWriteOptions(p.Args)
	if err != nil {
		return nil, err
	}
	if p.InputSchema != nil {
		logDebug("lights_update bind: mode=%s input=%v tx=%x", opts.ResultMode, names, p.TransactionOpaqueData)
	}
	schema, err := vgi.WriteResultSchema(opts.ResultMode, LIGHTS.Schema)
	if err != nil {
		return nil, err
	}
	return &vgi.BindResponse{OutputSchema: schema}, nil
}

func (lightsUpdateFn) NewState(_ *vgi.ProcessParams) (*NoState, error) { return &NoState{}, nil }

func (lightsUpdateFn) Finalize(context.Context, *vgi.ProcessParams, *NoState) ([]arrow.RecordBatch, error) {
	return nil, nil
}

func (lightsUpdateFn) Process(_ context.Context, p *vgi.ProcessParams, _ *NoState, batch arrow.RecordBatch, out *vgirpc.OutputCollector) error {
	opts, err := vgi.ParseWriteOptions(p.Args)
	if err != nil {
		return err
	}
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	current, err := loadLightRows(conn)
	if err != nil {
		return err
	}
	// In a transaction, compare with (and build on) the light as the
	// transaction sees it, staged changes included.
	if current, err = withPendingChanges(current, ci); err != nil {
		return err
	}
	byRowID := map[int64]row{}
	for _, r := range current {
		byRowID[r["rowid"].(int64)] = r
	}
	ridCol := column0(batch, "rowid")
	if ridCol == nil {
		return fmt.Errorf("lights_update: input has no rowid column")
	}
	type planned struct {
		old  row
		body omap
	}
	plan := make([]planned, 0, batch.NumRows())
	for i := range int(batch.NumRows()) {
		rid, _ := cellValue(ridCol, i).(int64)
		old, ok := byRowID[rid]
		if !ok {
			return hueErr("light with rowid %d no longer exists on the bridge", rid)
		}
		u, err := rowChange(batch, i, old)
		if err != nil {
			return err
		}
		body, err := updateBody(u)
		if err != nil {
			return err
		}
		plan = append(plan, planned{old, body})
	}
	// One send for the whole batch, so the sender can pipeline across lights
	// (a light's own requests, e.g. state then rename, stay in order).
	oldRows, newRows := make([]row, len(plan)), make([]row, len(plan))
	var reqs []stagedRequest
	for i, pl := range plan {
		for _, b := range updateRequests(pl.body) {
			reqs = append(reqs, stagedRequest{Kind: "light", Target: pl.old["light_id"].(string), Body: b})
		}
		oldRows[i], newRows[i] = pl.old, applyToRow(pl.old, pl.body)
	}
	if _, err := senderFor(ci, conn)(reqs); err != nil {
		return err
	}
	res, err := writeResultBatch(opts.ResultMode, oldRows, newRows)
	if err != nil {
		return err
	}
	return out.Emit(res)
}
