// Inventory tables: lights, rooms, scenes, sensors. `lights` also accepts
// UPDATE (lights_write.go). Port of hue-worker/src/inventory.ts.

package hue

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
)

// colSpec is [name, Arrow type, SQL type, description, nullable].
type colSpec struct {
	Name     string
	Type     arrow.DataType
	SQLType  string
	Doc      string
	Nullable bool
}

type resultCols struct {
	Schema *arrow.Schema
	Names  []string
	Tag    string
}

func makeResultCols(specs []colSpec) resultCols {
	fields := make([]arrow.Field, len(specs))
	names := make([]string, len(specs))
	tag := make([]resultColumn, len(specs))
	for i, s := range specs {
		fields[i] = column(s.Name, s.Type, s.Doc, s.Nullable)
		names[i] = s.Name
		tag[i] = resultColumn{Name: s.Name, Type: s.SQLType, Description: s.Doc}
	}
	return resultCols{Schema: arrow.NewSchema(fields, nil), Names: names, Tag: mustJSON(tag)}
}

var (
	utf8T  = arrow.BinaryTypes.String
	boolT  = arrow.FixedWidthTypes.Boolean
	f64T   = arrow.PrimitiveTypes.Float64
	i32T   = arrow.PrimitiveTypes.Int32
	i64T   = arrow.PrimitiveTypes.Int64
	tsUTCT = &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
)

// rowIDField is DuckDB's conventional rowid (vgi.RowIDField), with a comment.
func rowIDField() arrow.Field {
	f := vgi.RowIDField("rowid", i64T)
	f.Metadata = arrow.NewMetadata(append(f.Metadata.Keys(), "comment"),
		append(f.Metadata.Values(), "Stable row id of the light (a hash of light_id)."))
	return f
}

// LIGHTS is the lights table's user columns (no rowid).
var LIGHTS = makeResultCols([]colSpec{
	{"light_id", utf8T, "VARCHAR", "Light id (CLIP v2 UUID); pass it to set_light.", false},
	{"name", utf8T, "VARCHAR", "Light name set in the Hue app.", true},
	{"room", utf8T, "VARCHAR", "Name of the room the light's device is in, or NULL.", true},
	{"archetype", utf8T, "VARCHAR", "Hue archetype, e.g. 'sultan_bulb', 'pendant_round'.", true},
	{"is_on", boolT, "BOOLEAN", "Whether the light is on.", true},
	{"brightness", f64T, "DOUBLE", "Brightness in percent (0–100); NULL for on/off-only lights.", true},
	{"color_mode", utf8T, "VARCHAR", "'color_temperature', 'xy', or NULL for white-only/dimmable lights.", true},
	{"color_temperature_kelvin", i32T, "INTEGER", "Colour temperature in Kelvin when in color_temperature mode.", true},
	{"color_x", f64T, "DOUBLE", "CIE 1931 x chromaticity coordinate of the current colour, 0–1.", true},
	{"color_y", f64T, "DOUBLE", "CIE 1931 y chromaticity coordinate of the current colour, 0–1.", true},
	{"color_hex", utf8T, "VARCHAR", "Approximate current colour as '#rrggbb' (full brightness).", true},
	{"reachable", boolT, "BOOLEAN", "Whether the bridge can currently reach the light over Zigbee.", true},
	{"product_name", utf8T, "VARCHAR", "Product name, e.g. 'Hue color lamp'.", true},
	{"model_id", utf8T, "VARCHAR", "Hardware model id, e.g. 'LCA001'.", true},
	{"device_id", utf8T, "VARCHAR", "Id of the physical device that owns the light.", true},
})

var lightsTableSchema = arrow.NewSchema(append(append([]arrow.Field{}, LIGHTS.Schema.Fields()...), rowIDField()), nil)

// ROOMS is the rooms table.
var ROOMS = makeResultCols([]colSpec{
	{"room_id", utf8T, "VARCHAR", "Room or zone id (CLIP v2 UUID).", false},
	{"name", utf8T, "VARCHAR", "Room or zone name; pass it to set_room.", true},
	{"kind", utf8T, "VARCHAR", "'room' or 'zone'.", true},
	{"archetype", utf8T, "VARCHAR", "Hue archetype, e.g. 'kitchen', 'living_room'.", true},
	{"grouped_light_id", utf8T, "VARCHAR", "Id of the grouped_light service that controls every light at once.", true},
	{"any_on", boolT, "BOOLEAN", "Whether at least one light in the room or zone is on.", true},
	{"brightness", f64T, "DOUBLE", "Average brightness of the lights that are on, in percent; 0 when all are off.", true},
	{"light_count", i32T, "INTEGER", "Number of lights in the room or zone.", true},
	{"light_names", arrow.ListOf(utf8T), "VARCHAR[]", "Names of the lights, sorted.", true},
})

// SCENES is the scenes table.
var SCENES = makeResultCols([]colSpec{
	{"scene_id", utf8T, "VARCHAR", "Scene id (CLIP v2 UUID); pass it to recall_scene.", false},
	{"name", utf8T, "VARCHAR", "Scene name, e.g. 'Relax'.", true},
	{"room", utf8T, "VARCHAR", "Name of the room or zone the scene belongs to.", true},
	{"room_id", utf8T, "VARCHAR", "Id of the room or zone the scene belongs to.", true},
	{"active", boolT, "BOOLEAN", "Whether the scene is currently active (static or dynamic).", true},
})

// SENSORS is the sensors table.
var SENSORS = makeResultCols([]colSpec{
	{"sensor_id", utf8T, "VARCHAR", "Sensor service id (CLIP v2 UUID).", false},
	{"kind", utf8T, "VARCHAR", "'motion', 'temperature', 'light_level', 'contact' or 'device_power' (battery).", true},
	{"device_id", utf8T, "VARCHAR", "Id of the physical device; one device can carry several sensors.", true},
	{"device_name", utf8T, "VARCHAR", "Name of the device, e.g. 'Hallway sensor'.", true},
	{"room", utf8T, "VARCHAR", "Name of the room the device is in, or NULL.", true},
	{"enabled", boolT, "BOOLEAN", "Whether the sensor is enabled (NULL for battery readings).", true},
	{"motion", boolT, "BOOLEAN", "motion: whether motion is currently detected.", true},
	{"temperature_c", f64T, "DOUBLE", "temperature: degrees Celsius.", true},
	{"light_level_lux", f64T, "DOUBLE", "light_level: ambient light in lux.", true},
	{"contact", boolT, "BOOLEAN", "contact: true when closed (in contact), false when open.", true},
	{"battery_percent", i32T, "INTEGER", "device_power: battery level in percent.", true},
	{"battery_state", utf8T, "VARCHAR", "device_power: 'normal', 'low' or 'critical'.", true},
	{"reachable", boolT, "BOOLEAN", "Whether the bridge can currently reach the device over Zigbee.", true},
	{"last_changed", tsUTCT, "TIMESTAMP WITH TIME ZONE", "When the reading last changed.", true},
})

var sensorKinds = []string{"motion", "temperature", "light_level", "contact", "device_power"}

func strp(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// ---------------------------------------------------------------- lights

// lightRowID: FNV-1a 64 of the light id, masked to 62 bits (positive BIGINT).
func lightRowID(id string) int64 {
	h := uint64(0xcbf29ce484222325)
	for _, b := range []byte(id) {
		h ^= uint64(b)
		h *= 0x100000001b3
	}
	return int64(h & 0x3fffffffffffffff)
}

func lightRows(lights []hueLight, devices []hueDevice, groups []hueGroup, zigbee []hueZigbee) ([]row, error) {
	devByID := map[string]hueDevice{}
	for _, d := range devices {
		devByID[d.ID] = d
	}
	rooms := roomByDevice(groups)
	reach := reachableByDevice(zigbee)
	sorted := append([]hueLight(nil), lights...)
	sort.SliceStable(sorted, func(i, j int) bool { return nameOf(sorted[i].Metadata) < nameOf(sorted[j].Metadata) })
	seen := map[int64]string{}
	out := make([]row, 0, len(sorted))
	for _, l := range sorted {
		rid := lightRowID(l.ID)
		if c, ok := seen[rid]; ok {
			return nil, fmt.Errorf("lights %s and %s hash to the same rowid; please report this", c, l.ID)
		}
		seen[rid] = l.ID
		r := row{"rowid": rid, "light_id": l.ID, "name": strp(l.Metadata.name()), "archetype": strp(l.Metadata.archetype()),
			"device_id": l.Owner.Rid}
		if g, ok := rooms[l.Owner.Rid]; ok {
			r["room"] = strp(g.Metadata.name())
		}
		if l.On != nil {
			r["is_on"] = l.On.On
		}
		if l.Dimming != nil {
			r["brightness"] = l.Dimming.Brightness
		}
		var mirek *float64
		if l.ColorTemperature != nil {
			mirek = l.ColorTemperature.Mirek
		}
		hasXY := l.Color != nil && l.Color.XY != nil
		switch {
		case mirek != nil:
			k := mirekToKelvin(*mirek)
			r["color_mode"], r["color_temperature_kelvin"], r["color_hex"] = "color_temperature", k, kelvinToHex(float64(k))
		case hasXY:
			r["color_mode"], r["color_hex"] = "xy", xyToHex(l.Color.XY.X, l.Color.XY.Y)
		}
		if hasXY {
			r["color_x"], r["color_y"] = l.Color.XY.X, l.Color.XY.Y
		}
		if v, ok := reach[l.Owner.Rid]; ok {
			r["reachable"] = v
		}
		if d, ok := devByID[l.Owner.Rid]; ok && d.ProductData != nil {
			r["product_name"], r["model_id"] = strp(d.ProductData.ProductName), strp(d.ProductData.ModelID)
		}
		out = append(out, r)
	}
	return out, nil
}

func loadLightRows(conn BridgeConnection) ([]row, error) {
	var lights []hueLight
	var devices []hueDevice
	var rooms []hueGroup
	var zigbee []hueZigbee
	if err := parallel(
		func() (e error) { lights, e = getResources[hueLight](conn, "light"); return },
		func() (e error) { devices, e = getResources[hueDevice](conn, "device"); return },
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zigbee, e = getResources[hueZigbee](conn, "zigbee_connectivity"); return },
	); err != nil {
		return nil, err
	}
	return lightRows(lights, devices, rooms, zigbee)
}

// applyToRow returns the row as it reads once the bridge has applied body.
func applyToRow(old row, body omap) row {
	r := row{}
	for k, v := range old {
		r[k] = v
	}
	if on := body.sub("on"); on != nil {
		if v, ok := on.get("on"); ok {
			r["is_on"] = v
		}
	}
	if d := body.sub("dimming"); d != nil {
		if v, ok := d.get("brightness"); ok {
			r["brightness"], _ = num(v)
		}
	}
	if ct := body.sub("color_temperature"); ct != nil {
		if v, ok := ct.get("mirek"); ok {
			m, _ := num(v)
			k := mirekToKelvin(m)
			r["color_mode"], r["color_temperature_kelvin"], r["color_hex"] = "color_temperature", k, kelvinToHex(float64(k))
		}
	}
	if c := body.sub("color"); c != nil {
		xy := c.sub("xy")
		xv, _ := xy.get("x")
		yv, _ := xy.get("y")
		x, _ := num(xv)
		y, _ := num(yv)
		r["color_mode"], r["color_temperature_kelvin"], r["color_x"], r["color_y"], r["color_hex"] = "xy", nil, x, y, xyToHex(x, y)
	}
	if md := body.sub("metadata"); md != nil {
		if v, ok := md.get("name"); ok {
			r["name"] = v
		}
	}
	return r
}

// withPendingChanges overlays the transaction's staged light changes.
func withPendingChanges(rows []row, ci callInfo) ([]row, error) {
	tx := stageable(ci.Opts.Transactional, ci.Tx)
	if tx == nil {
		return rows, nil
	}
	pending, err := pendingLightBodies(tx)
	if err != nil || len(pending) == 0 {
		return rows, err
	}
	for i, r := range rows {
		id, _ := r["light_id"].(string)
		for _, b := range pending[id] {
			r = applyToRow(r, b)
		}
		rows[i] = r
	}
	return rows, nil
}

// ---------------------------------------------------------------- rooms

func roomRows(groups []hueGroup, grouped []hueGroupedLight, lights []hueLight) []row {
	glByID := map[string]hueGroupedLight{}
	for _, g := range grouped {
		glByID[g.ID] = g
	}
	sorted := append([]hueGroup(nil), groups...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Type != sorted[j].Type {
			return sorted[i].Type < sorted[j].Type
		}
		return nameOf(sorted[i].Metadata) < nameOf(sorted[j].Metadata)
	})
	var out []row
	for _, g := range sorted {
		r := row{"room_id": g.ID, "name": strp(g.Metadata.name()), "kind": g.Type, "archetype": strp(g.Metadata.archetype())}
		for _, s := range g.Services {
			if s.Rtype == "grouped_light" {
				r["grouped_light_id"] = s.Rid
				if gl, ok := glByID[s.Rid]; ok {
					if gl.On != nil {
						r["any_on"] = gl.On.On
					}
					if gl.Dimming != nil {
						r["brightness"] = gl.Dimming.Brightness
					}
				}
				break
			}
		}
		members := lightsInGroup(g, lights)
		names := make([]string, 0, len(members))
		for _, l := range members {
			n := l.ID
			if l.Metadata.name() != nil {
				n = *l.Metadata.name()
			}
			names = append(names, n)
		}
		r["light_count"] = int32(len(members))
		r["light_names"] = sortedNames(names)
		out = append(out, r)
	}
	return out
}

func loadRooms(conn BridgeConnection) ([]row, error) {
	var rooms, zones []hueGroup
	var grouped []hueGroupedLight
	var lights []hueLight
	if err := parallel(
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zones, e = getResources[hueGroup](conn, "zone"); return },
		func() (e error) { grouped, e = getResources[hueGroupedLight](conn, "grouped_light"); return },
		func() (e error) { lights, e = getResources[hueLight](conn, "light"); return },
	); err != nil {
		return nil, err
	}
	return roomRows(append(rooms, zones...), grouped, lights), nil
}

// ---------------------------------------------------------------- scenes

func loadScenes(conn BridgeConnection) ([]row, error) {
	var scenes []hueScene
	var rooms, zones []hueGroup
	if err := parallel(
		func() (e error) { scenes, e = getResources[hueScene](conn, "scene"); return },
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zones, e = getResources[hueGroup](conn, "zone"); return },
	); err != nil {
		return nil, err
	}
	byID := map[string]hueGroup{}
	for _, g := range append(rooms, zones...) {
		byID[g.ID] = g
	}
	out := make([]row, 0, len(scenes))
	for _, s := range scenes {
		r := row{"scene_id": s.ID, "name": strp(s.Metadata.name())}
		if s.Group != nil {
			r["room_id"] = s.Group.Rid
			if g, ok := byID[s.Group.Rid]; ok {
				r["room"] = strp(g.Metadata.name())
			}
		}
		if s.Status != nil && s.Status.Active != nil && *s.Status.Active != "" {
			r["active"] = *s.Status.Active != "inactive"
		}
		out = append(out, r)
	}
	str := func(v any) string { s, _ := v.(string); return s }
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := str(out[i]["room"]), str(out[j]["room"]); a != b {
			return a < b
		}
		return str(out[i]["name"]) < str(out[j]["name"])
	})
	return out, nil
}

// ---------------------------------------------------------------- sensors

func micros(s *string) any {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	return t.UnixMilli() * 1000
}

func loadSensors(conn BridgeConnection) ([]row, error) {
	var devices []hueDevice
	var rooms []hueGroup
	var zigbee []hueZigbee
	byKind := make([][]hueSensor, len(sensorKinds))
	fns := []func() error{
		func() (e error) { devices, e = getResources[hueDevice](conn, "device"); return },
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zigbee, e = getResources[hueZigbee](conn, "zigbee_connectivity"); return },
	}
	for i, k := range sensorKinds {
		fns = append(fns, func() (e error) { byKind[i], e = getResources[hueSensor](conn, k); return })
	}
	if err := parallel(fns...); err != nil {
		return nil, err
	}
	devByID := map[string]hueDevice{}
	for _, d := range devices {
		devByID[d.ID] = d
	}
	roomOf := roomByDevice(rooms)
	reach := reachableByDevice(zigbee)
	var out []row
	for _, list := range byKind {
		for _, s := range list {
			live := s.Enabled == nil || *s.Enabled
			r := row{"sensor_id": s.ID, "kind": s.Type, "device_id": s.Owner.Rid}
			if d, ok := devByID[s.Owner.Rid]; ok {
				r["device_name"] = strp(d.Metadata.name())
			}
			if g, ok := roomOf[s.Owner.Rid]; ok {
				r["room"] = strp(g.Metadata.name())
			}
			if s.Enabled != nil {
				r["enabled"] = *s.Enabled
			}
			var changed *string
			if live && s.Motion != nil {
				if s.Motion.MotionReport != nil && s.Motion.MotionReport.Motion != nil {
					r["motion"] = *s.Motion.MotionReport.Motion
				} else if s.Motion.Motion != nil {
					r["motion"] = *s.Motion.Motion
				}
				if s.Motion.MotionReport != nil {
					changed = s.Motion.MotionReport.Changed
				}
			}
			if live && s.Temperature != nil {
				if s.Temperature.TemperatureReport != nil && s.Temperature.TemperatureReport.Temperature != nil {
					r["temperature_c"] = *s.Temperature.TemperatureReport.Temperature
				} else if s.Temperature.Temperature != nil {
					r["temperature_c"] = *s.Temperature.Temperature
				}
				if changed == nil && s.Temperature.TemperatureReport != nil {
					changed = s.Temperature.TemperatureReport.Changed
				}
			}
			if live && s.Light != nil {
				var level *float64
				if s.Light.LightLevelReport != nil && s.Light.LightLevelReport.LightLevel != nil {
					level = s.Light.LightLevelReport.LightLevel
				} else {
					level = s.Light.LightLevel
				}
				if level != nil {
					r["light_level_lux"] = lightLevelToLux(*level)
				}
				if changed == nil && s.Light.LightLevelReport != nil {
					changed = s.Light.LightLevelReport.Changed
				}
			}
			if live && s.ContactReport != nil {
				if s.ContactReport.State != nil && *s.ContactReport.State != "" {
					r["contact"] = *s.ContactReport.State == "contact"
				}
				if changed == nil {
					changed = s.ContactReport.Changed
				}
			}
			if s.PowerState != nil {
				if s.PowerState.BatteryLevel != nil {
					r["battery_percent"] = *s.PowerState.BatteryLevel
				}
				r["battery_state"] = strp(s.PowerState.BatteryState)
			}
			if v, ok := reach[s.Owner.Rid]; ok {
				r["reachable"] = v
			}
			if live {
				r["last_changed"] = micros(changed)
			}
			out = append(out, r)
		}
	}
	str := func(v any) string { s, _ := v.(string); return s }
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := str(out[i]["device_name"]), str(out[j]["device_name"]); a != b {
			return a < b
		}
		return str(out[i]["kind"]) < str(out[j]["kind"])
	})
	return out, nil
}

// ---------------------------------------------------------------- scans

type inventory struct {
	name       string
	comment    string
	cols       resultCols
	schema     *arrow.Schema // table columns (lights: + rowid)
	primaryKey string
	notNull    []string
	rowID      bool
	load       func(conn BridgeConnection, ci callInfo) ([]row, error)
	tags       map[string]string
}

// DoneState is the one-shot scan cursor.
type DoneState struct{ Done bool }

type scanFn struct{ inv *inventory }

var _ vgi.TypedTableFunc[DoneState] = scanFn{}

func (f scanFn) Name() string { return f.inv.name + "_scan" }

func (f scanFn) Metadata() vgi.FunctionMetadata {
	md := vgi.FunctionMetadata{Description: "Backing scan for the " + f.inv.name + " table", Stability: vgi.StabilityVolatile}
	if f.inv.rowID {
		md.Description = "Every row of the " + f.inv.name + " table, with its rowid (what UPDATE uses)"
		md.ProjectionPushdown = true
		tags := objectTags(docs{Title: "Lights (scan with rowid)", Category: "inventory", Keywords: "hue, lights, scan, rowid, update",
			DocLLM: "The table function behind `hue.main.lights`: the same rows, plus DuckDB's `rowid`. " +
				"Query the `hue.main.lights` table instead; this function exists so DuckDB can scan the table for UPDATE.",
			DocMD: "## lights_scan()\n\nThe scan behind `hue.main.lights`; query the table instead."})
		md.Tags = tags
	}
	return md
}

func (f scanFn) ArgumentSpecs() []vgi.ArgSpec { return nil }

func (f scanFn) OnBind(_ *vgi.BindParams) (*vgi.BindResponse, error) {
	return &vgi.BindResponse{OutputSchema: f.inv.schema}, nil
}

func (f scanFn) NewState(_ *vgi.ProcessParams) (*DoneState, error) { return &DoneState{}, nil }

func (f scanFn) Process(_ context.Context, p *vgi.ProcessParams, st *DoneState, out *vgirpc.OutputCollector) error {
	if st.Done {
		return out.Finish()
	}
	ci := callInfoOf(p)
	conn, err := ci.connect()
	if err != nil {
		return err
	}
	rows, err := f.inv.load(conn, ci)
	if err != nil {
		return err
	}
	s := p.OutputSchema
	if s == nil {
		s = f.inv.schema
	}
	names := make([]string, s.NumFields())
	for i, fl := range s.Fields() {
		names[i] = fl.Name
	}
	batch, err := buildBatch(s, rowsToColumns(rows, names), len(rows))
	if err != nil {
		return err
	}
	st.Done = true
	return out.Emit(batch)
}

func (inv *inventory) constraints() [][]byte {
	var out [][]byte
	add := func(id string, kind vgi.ConstraintKind, col string) {
		rec, err := vgi.ConstraintRecord(vgi.ConstraintRecordSpec{SchemaPath: []string{"main"}, Table: inv.name, ID: id, Kind: kind,
			Columns: [][]string{{col}}, Enforcement: vgi.ConstraintEnforced, Validation: vgi.ConstraintAllRows})
		if err != nil {
			panic(err)
		}
		out = append(out, rec)
	}
	for _, c := range append([]string{inv.primaryKey}, inv.notNull...) {
		add(c+"_not_null", vgi.ConstraintNotNull, c)
	}
	add("pk", vgi.ConstraintPrimaryKey, inv.primaryKey)
	return out
}

func (inv *inventory) catalogTable() vgi.CatalogTable {
	t := vgi.CatalogTable{Name: inv.name, Comment: inv.comment, Columns: inv.schema, Function: vgi.AsTableFunction[DoneState](scanFn{inv}),
		Constraints: inv.constraints(), Tags: inv.tags}
	if inv.rowID {
		// UPDATE hue.main.lights; RETURNING needs at least "rows".
		t.UpdateFunction = vgi.AsTableInOutFunction[NoState](lightsUpdateFn{})
		t.WriteResultModes = map[vgi.WriteOp]vgi.WriteResultMode{vgi.WriteOpUpdate: vgi.WriteResultChanges}
	}
	return t
}

func tableTags(title, category, keywords, docLLM string, examples []exampleQuery) map[string]string {
	t := objectTags(docs{Title: title, Category: category, Keywords: keywords, DocLLM: docLLM, DocMD: "## " + strings.ToLower(title) + "\n\n" + docLLM})
	t["domain"] = "smart home"
	t["vgi.example_queries"] = mustJSON(examples)
	return t
}

var lightsInv = &inventory{
	name: "lights", comment: "Every light on the Hue bridge with its room, on/off state, brightness and colour",
	cols: LIGHTS, schema: lightsTableSchema, primaryKey: "light_id", notNull: []string{"device_id"}, rowID: true,
	load: func(conn BridgeConnection, ci callInfo) ([]row, error) {
		rows, err := loadLightRows(conn)
		if err != nil {
			return nil, err
		}
		return withPendingChanges(rows, ci)
	},
	tags: tableTags("Hue Lights", "inventory", "hue, lights, bulbs, brightness, colour, state",
		"Every light on the attached Hue bridge with its current state. Writable with UPDATE: "+
			"is_on, brightness, color_temperature_kelvin, color_hex and name.",
		[]exampleQuery{{Description: "How many lights are on in each room",
			SQL: "SELECT room, count(*) FILTER (WHERE is_on) AS lit FROM hue.main.lights GROUP BY room ORDER BY room"}}),
}

var roomsInv = &inventory{
	name: "rooms", comment: "Rooms and zones on the Hue bridge with their lights and combined state",
	cols: ROOMS, schema: ROOMS.Schema, primaryKey: "room_id", notNull: []string{"kind", "light_count", "light_names"},
	load: func(conn BridgeConnection, _ callInfo) ([]row, error) { return loadRooms(conn) },
	tags: tableTags("Rooms and Zones", "inventory", "hue, rooms, zones, groups",
		"Rooms and zones, their lights, and whether any are on.",
		[]exampleQuery{{Description: "Rooms with the most lights first",
			SQL: "SELECT name, light_count FROM hue.main.rooms WHERE kind = 'room' ORDER BY light_count DESC"}}),
}

var scenesInv = &inventory{
	name: "scenes", comment: "Scenes stored on the Hue bridge, with their room and whether each is active",
	cols: SCENES, schema: SCENES.Schema, primaryKey: "scene_id",
	load: func(conn BridgeConnection, _ callInfo) ([]row, error) { return loadScenes(conn) },
	tags: tableTags("Hue Scenes", "inventory", "hue, scenes, presets",
		"Scenes on the bridge and the room each belongs to.",
		[]exampleQuery{{Description: "Which scene is active in each room",
			SQL: "SELECT room, name FROM hue.main.scenes WHERE active ORDER BY room"}}),
}

var sensorsInv = &inventory{
	name: "sensors", comment: "Motion, temperature, light-level, contact and battery readings from Hue sensors",
	cols: SENSORS, schema: SENSORS.Schema, primaryKey: "sensor_id", notNull: []string{"kind", "device_id"},
	load: func(conn BridgeConnection, _ callInfo) ([]row, error) { return loadSensors(conn) },
	tags: tableTags("Hue Sensor Readings", "sensors", "hue, sensors, motion, temperature, battery",
		"Current readings from motion, temperature, light-level, contact and battery sensors, one row per reading.",
		[]exampleQuery{{Description: "Average temperature per room",
			SQL: "SELECT room, avg(temperature_c) FROM hue.main.sensors WHERE kind = 'temperature' GROUP BY room ORDER BY room"}}),
}

var inventories = []*inventory{lightsInv, roomsInv, scenesInv, sensorsInv}
