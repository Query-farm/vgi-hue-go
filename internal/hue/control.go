// Control functions: set_light, set_room, recall_scene and the table in-out
// set_lights. Port of hue-worker/src/control.ts.

package hue

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
)

const (
	maxWaitMs    = 30_000
	settlePollMs = 250
)

func fp(v float64) *float64 { return &v }

// stateArgs are the control functions' named state arguments. Each is
// optional and defaults to NULL (a nil pointer): NULL leaves that part of the
// state unchanged.
type stateArgs struct {
	Power        *bool    `vgi:"name=power"`
	Brightness   *float64 `vgi:"name=brightness,ge=0,le=100"`
	Kelvin       *int64   `vgi:"name=kelvin,ge=2000,le=6500"`
	Color        *string  `vgi:"name=color"`
	TransitionMs *int64   `vgi:"name=transition_ms,ge=0,le=3600000"`
}

var stateArgSpecs = withDocs(vgi.DeriveArgSpecs(stateArgs{}), map[string]string{
	"power":         "Named, optional: true turns on, false turns off; NULL leaves it unchanged.",
	"brightness":    "Named, optional: brightness in percent, 0–100. Above 0 also turns the light on; 0 turns it off.",
	"kelvin":        "Named, optional: white colour temperature in Kelvin, 2000 (warm) to 6500 (cool).",
	"color":         "Named, optional: colour as '#rrggbb' or a basic colour name.",
	"transition_ms": "Named, optional: fade duration in milliseconds.",
})

// withDocs sets each derived spec's Doc by name (docs carry commas and quotes,
// which are awkward inside struct tags).
func withDocs(specs []vgi.ArgSpec, docs map[string]string) []vgi.ArgSpec {
	for i := range specs {
		specs[i].Doc = docs[specs[i].Name]
	}
	return specs
}

func changeFrom(args *vgi.Arguments) (lightChange, error) {
	var a stateArgs
	if err := vgi.BindArgs(args, &a); err != nil {
		return lightChange{}, err
	}
	c := lightChange{On: a.Power, Brightness: a.Brightness, Color: a.Color}
	if a.Kelvin != nil {
		c.Kelvin = fp(float64(*a.Kelvin))
	}
	if a.TransitionMs != nil {
		c.TransitionMs = fp(float64(*a.TransitionMs))
	}
	return c, nil
}

// RESULT is the control functions' output.
var RESULT = makeResultCols([]colSpec{
	{"target_type", utf8T, "VARCHAR", "What was changed: 'light', 'room', 'zone' or 'scene'.", false},
	{"target_id", utf8T, "VARCHAR", "Id of the light, room/zone, or scene.", false},
	{"name", utf8T, "VARCHAR", "Name of the light, room/zone, or scene.", true},
	{"ok", boolT, "BOOLEAN", "Whether the bridge accepted the change.", false},
	{"error", utf8T, "VARCHAR", "Why the change failed, or NULL.", true},
	{"request", utf8T, "VARCHAR", "JSON body sent to the bridge.", true},
	{"settled", boolT, "BOOLEAN", "set_room with wait_ms: whether every reachable light reported the new state; NULL when not waited.", true},
})

func resultRow(typ, id string, name *string, ok bool, errMsg *string, body omap, settled *bool) row {
	r := row{"target_type": typ, "target_id": id, "name": strp(name), "ok": ok, "error": strp(errMsg)}
	if body != nil {
		b, _ := json.Marshal(body)
		r["request"] = string(b)
	}
	if settled != nil {
		r["settled"] = *settled
	}
	return r
}

func emitResult(out *vgirpc.OutputCollector, s *arrow.Schema, rows []row) error {
	if s == nil {
		s = RESULT.Schema
	}
	names := make([]string, s.NumFields())
	for i, f := range s.Fields() {
		names[i] = f.Name
	}
	b, err := buildBatch(s, rowsToColumns(rows, names), len(rows))
	if err != nil {
		return err
	}
	return out.Emit(b)
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func matchByIDOrName[T any](items []T, id func(T) string, md func(T) *metadata, target string) []T {
	var out []T
	for _, it := range items {
		if id(it) == target {
			out = append(out, it)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, it := range items {
		if norm(nameOf(md(it))) == norm(target) {
			out = append(out, it)
		}
	}
	return out
}

func known[T any](items []T, md func(T) *metadata) string {
	var names []string
	for _, it := range items {
		if n := nameOf(md(it)); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) > 20 {
		names = names[:20]
	}
	for i := range names {
		names[i] = "'" + names[i] + "'"
	}
	return strings.Join(names, ", ")
}

var (
	lightID  = func(l hueLight) string { return l.ID }
	lightMD  = func(l hueLight) *metadata { return l.Metadata }
	groupID  = func(g hueGroup) string { return g.ID }
	groupMD  = func(g hueGroup) *metadata { return g.Metadata }
	sceneID  = func(s hueScene) string { return s.ID }
	sceneMD  = func(s hueScene) *metadata { return s.Metadata }
	controlT = func(title, keywords, doc string) map[string]string {
		t := objectTags(docs{Title: title, Category: "control", Keywords: keywords, DocLLM: doc, DocMD: "## " + title + "\n\n" + doc})
		t["vgi.result_columns_schema"] = RESULT.Tag
		return t
	}
)

// ------------------------------------------------------------------ set_light

type setLightFn struct{}

var _ vgi.TypedTableFunc[DoneState] = setLightFn{}

func (setLightFn) Name() string { return "set_light" }
func (setLightFn) Metadata() vgi.FunctionMetadata {
	return withExamples("set_light", vgi.FunctionMetadata{Description: "Turn one light on or off, or set its brightness, colour temperature or colour",
		Stability: vgi.StabilityVolatile, Tags: controlT("Set Light", "hue, light control, dim",
			"Change one light (by id or case-insensitive name). Named state arguments: power, brightness, kelvin, color, transition_ms.")})
}
func (setLightFn) ArgumentSpecs() []vgi.ArgSpec {
	return append([]vgi.ArgSpec{{Name: "light", Position: 0, ArrowType: "varchar", ArrowDataType: utf8T, IsConst: true,
		Doc: "Light id or name (case-insensitive); every light with that name is changed."}}, stateArgSpecs...)
}
func (setLightFn) OnBind(p *vgi.BindParams) (*vgi.BindResponse, error) {
	c, err := changeFrom(p.Args)
	if err != nil {
		return nil, err
	}
	if _, err := buildStateBody(c); err != nil {
		return nil, err
	}
	return &vgi.BindResponse{OutputSchema: RESULT.Schema}, nil
}
func (setLightFn) NewState(*vgi.ProcessParams) (*DoneState, error) { return &DoneState{}, nil }
func (setLightFn) Process(_ context.Context, p *vgi.ProcessParams, st *DoneState, out *vgirpc.OutputCollector) error {
	if st.Done {
		return out.Finish()
	}
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	target, err := p.Args.GetScalarString(0)
	if err != nil {
		return err
	}
	c, err := changeFrom(p.Args)
	if err != nil {
		return err
	}
	body, err := buildStateBody(c)
	if err != nil {
		return err
	}
	lights, err := getResources[hueLight](conn, "light")
	if err != nil {
		return err
	}
	matches := matchByIDOrName(lights, lightID, lightMD, target)
	if len(matches) == 0 {
		return hueErr("no light with id or name '%s'. Lights: %s", target, known(lights, lightMD))
	}
	send := senderFor(ci, conn)
	var rows []row
	for _, l := range matches {
		if _, err := send([]stagedRequest{{Kind: "light", Target: l.ID, Body: body}}); err != nil {
			return err
		}
		rows = append(rows, resultRow("light", l.ID, l.Metadata.name(), true, nil, body, nil))
	}
	st.Done = true
	return emitResult(out, p.OutputSchema, rows)
}

// ------------------------------------------------------------------ set_room

// lightReflects reports whether light shows the state body asked for.
func lightReflects(l hueLight, body omap) bool {
	if on := body.sub("on"); on != nil && l.On != nil {
		v, _ := on.get("on")
		if want, _ := v.(bool); l.On.On != want {
			return false
		}
		if want, _ := v.(bool); !want {
			return true
		}
	}
	if d := body.sub("dimming"); d != nil && l.Dimming != nil {
		v, _ := d.get("brightness")
		want, _ := num(v)
		if math.Abs(l.Dimming.Brightness-want) > 1.5 {
			return false
		}
	}
	if body.has("color_temperature") && l.ColorTemperature != nil && l.ColorTemperature.Mirek == nil {
		return false
	}
	if body.has("color") && l.Color != nil && l.ColorTemperature != nil && l.ColorTemperature.Mirek != nil {
		return false
	}
	return true
}

func waitForGroup(conn BridgeConnection, g hueGroup, body omap, waitMs int64) (bool, error) {
	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	zigbee, err := getResources[hueZigbee](conn, "zigbee_connectivity")
	if err != nil {
		return false, err
	}
	reach := reachableByDevice(zigbee)
	for {
		lights, err := getResources[hueLight](conn, "light")
		if err != nil {
			return false, err
		}
		all := true
		for _, l := range lightsInGroup(g, lights) {
			if r, ok := reach[l.Owner.Rid]; ok && !r {
				continue
			}
			if !lightReflects(l, body) {
				all = false
			}
		}
		if all {
			return true, nil
		}
		if time.Now().Add(settlePollMs * time.Millisecond).After(deadline) {
			return false, nil
		}
		time.Sleep(settlePollMs * time.Millisecond)
	}
}

type setRoomFn struct{}

var _ vgi.TypedTableFunc[DoneState] = setRoomFn{}

func (setRoomFn) Name() string { return "set_room" }
func (setRoomFn) Metadata() vgi.FunctionMetadata {
	return withExamples("set_room", vgi.FunctionMetadata{Description: "Change every light in a room or zone at once: on/off, brightness, colour temperature or colour",
		Stability: vgi.StabilityVolatile, Tags: controlT("Set Room", "hue, room, zone, group",
			"Change every light in one room or zone with a single command. wait_ms waits for the bulbs and reports it in settled.")})
}
func (setRoomFn) ArgumentSpecs() []vgi.ArgSpec {
	specs := append([]vgi.ArgSpec{{Name: "room", Position: 0, ArrowType: "varchar", ArrowDataType: utf8T, IsConst: true,
		Doc: "Room or zone id or name (case-insensitive), as listed by rooms."}}, stateArgSpecs...)
	return append(specs, vgi.ArgSpec{Name: "wait_ms", Position: -1, ArrowType: "int64", ArrowDataType: i64T, IsConst: true,
		Ge: fp(0), Le: fp(maxWaitMs), Doc: "Named, optional: wait up to this many milliseconds (max 30000) until every reachable light reports the new state."})
}

type waitArgs struct {
	WaitMs *int64 `vgi:"name=wait_ms"`
}

func waitMsArg(args *vgi.Arguments) (*int64, error) {
	var a waitArgs
	if err := vgi.BindArgs(args, &a); err != nil {
		return nil, err
	}
	if a.WaitMs != nil && (*a.WaitMs < 0 || *a.WaitMs > maxWaitMs) {
		return nil, hueErr("wait_ms must be 0–%d, got %d", maxWaitMs, *a.WaitMs)
	}
	return a.WaitMs, nil
}
func (setRoomFn) OnBind(p *vgi.BindParams) (*vgi.BindResponse, error) {
	c, err := changeFrom(p.Args)
	if err != nil {
		return nil, err
	}
	if _, err := buildStateBody(c); err != nil {
		return nil, err
	}
	if _, err := waitMsArg(p.Args); err != nil {
		return nil, err
	}
	return &vgi.BindResponse{OutputSchema: RESULT.Schema}, nil
}
func (setRoomFn) NewState(*vgi.ProcessParams) (*DoneState, error) { return &DoneState{}, nil }
func (setRoomFn) Process(_ context.Context, p *vgi.ProcessParams, st *DoneState, out *vgirpc.OutputCollector) error {
	if st.Done {
		return out.Finish()
	}
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	target, err := p.Args.GetScalarString(0)
	if err != nil {
		return err
	}
	c, err := changeFrom(p.Args)
	if err != nil {
		return err
	}
	body, err := buildStateBody(c)
	if err != nil {
		return err
	}
	waitMs, err := waitMsArg(p.Args)
	if err != nil {
		return err
	}
	var rooms, zones []hueGroup
	if err := parallel(
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zones, e = getResources[hueGroup](conn, "zone"); return },
	); err != nil {
		return err
	}
	groups := append(rooms, zones...)
	matches := matchByIDOrName(groups, groupID, groupMD, target)
	if len(matches) == 0 {
		return hueErr("no room or zone with id or name '%s'. Rooms and zones: %s", target, known(groups, groupMD))
	}
	if len(matches) > 1 {
		return hueErr("'%s' names both a room and a zone; pass the room_id from rooms instead", target)
	}
	g := matches[0]
	glID := ""
	for _, s := range g.Services {
		if s.Rtype == "grouped_light" {
			glID = s.Rid
			break
		}
	}
	if glID == "" {
		return hueErr("%s '%s' has no grouped_light service", g.Type, nameOf(g.Metadata))
	}
	wasStaged, err := senderFor(ci, conn)([]stagedRequest{{Kind: "grouped_light", Target: glID, Body: body}})
	if err != nil {
		return err
	}
	var settled *bool
	if waitMs != nil && !wasStaged {
		s, err := waitForGroup(conn, g, body, *waitMs)
		if err != nil {
			return err
		}
		settled = &s
	}
	st.Done = true
	return emitResult(out, p.OutputSchema, []row{resultRow(g.Type, g.ID, g.Metadata.name(), true, nil, body, settled)})
}

// -------------------------------------------------------------- recall_scene

type recallSceneFn struct{}

var _ vgi.TypedTableFunc[DoneState] = recallSceneFn{}

func (recallSceneFn) Name() string { return "recall_scene" }
func (recallSceneFn) Metadata() vgi.FunctionMetadata {
	return withExamples("recall_scene", vgi.FunctionMetadata{Description: "Activate a Hue scene by id, or by name optionally narrowed to a room",
		Stability: vgi.StabilityVolatile, Tags: controlT("Recall Scene", "hue, scene, preset",
			"Activate a scene by id or name; pass room := '...' when the name repeats across rooms.")})
}
func (recallSceneFn) ArgumentSpecs() []vgi.ArgSpec {
	return []vgi.ArgSpec{
		{Name: "scene", Position: 0, ArrowType: "varchar", ArrowDataType: utf8T, IsConst: true, Doc: "Scene id, or scene name (case-insensitive)."},
		{Name: "room", Position: -1, ArrowType: "varchar", ArrowDataType: utf8T, IsConst: true,
			Doc: "Named, optional: room or zone name or id, to pick between scenes that share a name."},
	}
}
func (recallSceneFn) OnBind(_ *vgi.BindParams) (*vgi.BindResponse, error) {
	return &vgi.BindResponse{OutputSchema: RESULT.Schema}, nil
}
func (recallSceneFn) NewState(*vgi.ProcessParams) (*DoneState, error) { return &DoneState{}, nil }
func (recallSceneFn) Process(_ context.Context, p *vgi.ProcessParams, st *DoneState, out *vgirpc.OutputCollector) error {
	if st.Done {
		return out.Finish()
	}
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	target, err := p.Args.GetScalarString(0)
	if err != nil {
		return err
	}
	var ra struct {
		Room *string `vgi:"name=room"`
	}
	if err := vgi.BindArgs(p.Args, &ra); err != nil {
		return err
	}
	room := ra.Room
	var scenes []hueScene
	var rooms, zones []hueGroup
	if err := parallel(
		func() (e error) { scenes, e = getResources[hueScene](conn, "scene"); return },
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zones, e = getResources[hueGroup](conn, "zone"); return },
	); err != nil {
		return err
	}
	matches := matchByIDOrName(scenes, sceneID, sceneMD, target)
	inRoom := ""
	if room != nil {
		inRoom = fmt.Sprintf(" in '%s'", *room)
		ids := map[string]bool{}
		for _, g := range matchByIDOrName(append(rooms, zones...), groupID, groupMD, *room) {
			ids[g.ID] = true
		}
		if len(ids) == 0 {
			return hueErr("no room or zone with id or name '%s'", *room)
		}
		var kept []hueScene
		for _, s := range matches {
			if s.Group != nil && ids[s.Group.Rid] {
				kept = append(kept, s)
			}
		}
		matches = kept
	}
	if len(matches) == 0 {
		return hueErr("no scene '%s'%s. Scenes: %s", target, inRoom, known(scenes, sceneMD))
	}
	if len(matches) > 1 {
		return hueErr("scene name '%s' exists in several rooms; add room := '...' or pass the scene_id", target)
	}
	s := matches[0]
	body := omap{{"recall", omap{{"action", "active"}}}}
	if _, err := senderFor(ci, conn)([]stagedRequest{{Kind: "scene", Target: s.ID, Body: body}}); err != nil {
		return err
	}
	st.Done = true
	return emitResult(out, p.OutputSchema, []row{resultRow("scene", s.ID, s.Metadata.name(), true, nil, body, nil)})
}

// ---------------------------------------------------------------- set_lights

type setLightsFn struct{}

var _ vgi.TypedTableInOutFunc[NoState] = setLightsFn{}

func (setLightsFn) Name() string { return "set_lights" }
func (setLightsFn) Metadata() vgi.FunctionMetadata {
	return withExamples("set_lights", vgi.FunctionMetadata{Description: "Apply one change to every light listed by a query (its light_id column)",
		Stability: vgi.StabilityVolatile, Tags: controlT("Set Lights (bulk)", "hue, bulk, many lights",
			"Apply the same change to every light a subquery returns (it needs a light_id column; name is echoed). "+
				"Per-light failures are reported in that light's row.")})
}
func (setLightsFn) ArgumentSpecs() []vgi.ArgSpec {
	return append([]vgi.ArgSpec{{Name: "data", Position: 0, ArrowType: "table",
		Doc: "Input query: one row per light to change, with a light_id column and optionally a name column."}}, stateArgSpecs...)
}
func (setLightsFn) OnBind(p *vgi.BindParams) (*vgi.BindResponse, error) {
	if p.InputSchema == nil || len(p.InputSchema.FieldIndices("light_id")) == 0 {
		return nil, hueErr("set_lights needs an input query with a light_id column, e.g. (SELECT light_id FROM hue.main.lights)")
	}
	c, err := changeFrom(p.Args)
	if err != nil {
		return nil, err
	}
	if _, err := buildStateBody(c); err != nil {
		return nil, err
	}
	return &vgi.BindResponse{OutputSchema: RESULT.Schema}, nil
}
func (setLightsFn) NewState(*vgi.ProcessParams) (*NoState, error) { return &NoState{}, nil }
func (setLightsFn) Finalize(context.Context, *vgi.ProcessParams, *NoState) ([]arrow.RecordBatch, error) {
	return nil, nil
}
func (setLightsFn) Process(_ context.Context, p *vgi.ProcessParams, _ *NoState, batch arrow.RecordBatch, out *vgirpc.OutputCollector) error {
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	send := senderFor(ci, conn)
	c, err := changeFrom(p.Args)
	if err != nil {
		return err
	}
	body, err := buildStateBody(c)
	if err != nil {
		return err
	}
	ids, names := column0(batch, "light_id"), column0(batch, "name")
	var rows []row
	for i := range int(batch.NumRows()) {
		var name *string
		if names != nil {
			if s, ok := cellValue(names, i).(string); ok {
				name = &s
			}
		}
		id, ok := cellValue(ids, i).(string)
		if !ok {
			msg := "light_id is NULL"
			rows = append(rows, resultRow("light", "", name, false, &msg, nil, nil))
			continue
		}
		if _, err := send([]stagedRequest{{Kind: "light", Target: id, Body: body}}); err != nil {
			msg := err.Error()
			rows = append(rows, resultRow("light", id, name, false, &msg, body, nil))
			continue
		}
		rows = append(rows, resultRow("light", id, name, true, nil, body, nil))
	}
	return emitResult(out, nil, rows)
}

// -------------------------------------------------------------- set_gradient

const defaultGradientMode = "interpolated_palette"

type setGradientArgs struct {
	Light  string   `vgi:"pos=0"`
	Colors []string `vgi:"pos=1"`
	Mode   *string  `vgi:"name=mode,default=interpolated_palette"`
}

var setGradientArgSpecs = withDocs(vgi.DeriveArgSpecs(setGradientArgs{}), map[string]string{
	"light":  "Light id or name (case-insensitive), as for set_light; every light with that name is changed. It must be a gradient light.",
	"colors": "The gradient points, first to last, as '#rrggbb' strings (or basic colour names): 2 to the light's gradient_points_capable.",
	"mode": "Named, optional: how the points are spread over the light. Each gradient light has its own set of modes; " +
		"hue.main.lights shows the current one in gradient_mode. Default interpolated_palette.",
})

// gradientArgs reads and checks set_gradient's arguments: the light, the
// points as xy and the mode (the light-specific checks need the light, so
// they run in Process).
func gradientArgs(args *vgi.Arguments) (string, []xyPoint, string, error) {
	light, err := args.GetScalarString(0)
	if err != nil {
		return "", nil, "", err
	}
	mode := defaultGradientMode
	var a struct {
		Mode *string `vgi:"name=mode"`
	}
	if err := vgi.BindArgs(args, &a); err != nil {
		return "", nil, "", err
	}
	if a.Mode != nil && *a.Mode != "" {
		mode = *a.Mode
	}
	// The list is read as is (not through BindArgs) so a NULL element is seen.
	var colors []any
	if col, err := args.GetColumn(1); err == nil && col.Len() > 0 {
		colors, _ = cellValue(col, 0).([]any)
	}
	if len(colors) == 0 {
		return "", nil, "", hueErr("set_gradient needs at least two colours")
	}
	points, err := hexesToXY(colors)
	return light, points, mode, err
}

type setGradientFn struct{}

var _ vgi.TypedTableFunc[DoneState] = setGradientFn{}

func (setGradientFn) Name() string { return "set_gradient" }
func (setGradientFn) Metadata() vgi.FunctionMetadata {
	tags := controlT("Set Gradient", "hue, gradient, lightstrip, colours, palette",
		"Set the gradient points of one gradient light (by id or case-insensitive name) from a list of '#rrggbb' colours, "+
			"with mode := 'interpolated_palette' (default), 'interpolated_palette_mirrored', 'random_pixelated' or 'segmented_palette'. "+
			"The light must have a gradient (gradient_points_capable in hue.main.lights is not NULL); 2 to gradient_points_capable colours. "+
			"The same as UPDATE hue.main.lights SET gradient_hex = [...], gradient_mode = '...'.")
	return withExamples("set_gradient", vgi.FunctionMetadata{Description: "Set the gradient of a gradient light (lightstrip, Play bar...) from a list of '#rrggbb' colours",
		Stability: vgi.StabilityVolatile, Tags: tags})
}
func (setGradientFn) ArgumentSpecs() []vgi.ArgSpec { return setGradientArgSpecs }
func (setGradientFn) OnBind(p *vgi.BindParams) (*vgi.BindResponse, error) {
	if _, _, _, err := gradientArgs(p.Args); err != nil {
		return nil, err
	}
	return &vgi.BindResponse{OutputSchema: RESULT.Schema}, nil
}
func (setGradientFn) NewState(*vgi.ProcessParams) (*DoneState, error) { return &DoneState{}, nil }
func (setGradientFn) Process(_ context.Context, p *vgi.ProcessParams, st *DoneState, out *vgirpc.OutputCollector) error {
	if st.Done {
		return out.Finish()
	}
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	target, points, mode, err := gradientArgs(p.Args)
	if err != nil {
		return err
	}
	lights, err := getResources[hueLight](conn, "light")
	if err != nil {
		return err
	}
	matches := matchByIDOrName(lights, lightID, lightMD, target)
	if len(matches) == 0 {
		return hueErr("no light with id or name '%s'. Lights: %s", target, known(lights, lightMD))
	}
	body := omap{{"gradient", gradientBody(points, mode)}}
	// Check every match before changing any.
	for _, l := range matches {
		name := l.ID
		if n := l.Metadata.name(); n != nil {
			name = *n
		}
		if err := checkGradient(name, l.Gradient, points, mode); err != nil {
			return err
		}
	}
	reqs := make([]stagedRequest, len(matches))
	rows := make([]row, len(matches))
	for i, l := range matches {
		reqs[i] = stagedRequest{Kind: "light", Target: l.ID, Body: body}
		rows[i] = resultRow("light", l.ID, l.Metadata.name(), true, nil, body, nil)
	}
	if _, err := senderFor(ci, conn)(reqs); err != nil {
		return err
	}
	st.Done = true
	return emitResult(out, p.OutputSchema, rows)
}
