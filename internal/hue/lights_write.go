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

var updatableLightColumns = []string{"is_on", "brightness", "color_temperature_kelvin", "color_hex", "name"}

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

// rowChange is what input row i asks to change relative to the current row.
func rowChange(batch arrow.RecordBatch, i int, current row) (lightChange, *string, error) {
	var ch lightChange
	var name *string
	var refused []string
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
		if v == nil {
			return ch, nil, hueErr("cannot SET %s = NULL on a light", f.Name)
		}
		switch f.Name {
		case "is_on":
			b, _ := v.(bool)
			ch.On = &b
		case "brightness":
			x, _ := num(v)
			ch.Brightness = &x
		case "color_temperature_kelvin":
			x, _ := num(v)
			ch.Kelvin = &x
		case "color_hex":
			s := fmt.Sprint(v)
			ch.Color = &s
		default:
			s := fmt.Sprint(v)
			name = &s
		}
	}
	if len(refused) > 0 {
		return ch, nil, hueErr("cannot UPDATE %s of hue.main.lights; updatable columns: %s",
			strings.Join(refused, ", "), strings.Join(updatableLightColumns, ", "))
	}
	if name != nil && (strings.TrimSpace(*name) == "" || len([]rune(*name)) > 32) {
		return ch, nil, hueErr("light names must be 1-32 characters, got '%s'", *name)
	}
	return ch, name, nil
}

func updateBody(ch lightChange, name *string) (omap, error) {
	body := omap{}
	if ch.On != nil || ch.Brightness != nil || ch.Kelvin != nil || ch.Color != nil {
		var err error
		if body, err = buildStateBody(ch); err != nil {
			return nil, err
		}
	}
	if name != nil {
		body = body.set("metadata", omap{{"name", *name}})
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
		ch, name, err := rowChange(batch, i, old)
		if err != nil {
			return err
		}
		body, err := updateBody(ch, name)
		if err != nil {
			return err
		}
		plan = append(plan, planned{old, body})
	}
	send := senderFor(ci, conn)
	oldRows, newRows := make([]row, len(plan)), make([]row, len(plan))
	for i, pl := range plan {
		var reqs []stagedRequest
		for _, b := range updateRequests(pl.body) {
			reqs = append(reqs, stagedRequest{Kind: "light", Target: pl.old["light_id"].(string), Body: b})
		}
		if _, err := send(reqs); err != nil {
			return err
		}
		oldRows[i], newRows[i] = pl.old, applyToRow(pl.old, pl.body)
	}
	res, err := writeResultBatch(opts.ResultMode, oldRows, newRows)
	if err != nil {
		return err
	}
	return out.Emit(res)
}
