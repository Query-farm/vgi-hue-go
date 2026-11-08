// ATTACH options: bridge, app_key (secret), transactional.
//
// The validator packs the supplied values into attach_opaque_data, which DuckDB
// hands back on every call (sealed by the SDK over HTTP). Functions read it
// from BindParams.AttachOpaqueData (bind) or ProcessParams.AttachScope (process),
// and the transaction hooks from TransactionHookContext.AttachOpaqueData.

package hue

import (
	"bytes"
	"encoding/json"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// attachMagic opens our attach plaintext. It starts with the catalog name and
// a NUL (the SDK's catalogNameOf convention) so catalog-scoped lookups work.
var attachMagic = []byte(CatalogName + "\x00hue1:")

func attachOptionSpecs() []vgi.AttachOptionSpec {
	b := array.NewBooleanBuilder(mem)
	b.Append(false)
	falseArr := b.NewArray()
	b.Release()
	defBatch := array.NewRecordBatch(arrow.NewSchema([]arrow.Field{{Name: "value", Type: arrow.FixedWidthTypes.Boolean}}, nil),
		[]arrow.Array{falseArr}, 1)
	falseArr.Release()
	return []vgi.AttachOptionSpec{
		{Name: "bridge", Type: arrow.BinaryTypes.String, Description: "Hue bridge: an IP or host name (HTTPS is used), a full http(s):// URL, " +
			"or a bridge id to pick one of several. Defaults to the HUE_BRIDGE environment variable of the worker; " +
			"when neither is set the bridge is discovered on the local network (mDNS, then discovery.meethue.com)."},
		{Name: "app_key", Type: arrow.BinaryTypes.String, Secret: true, Description: "Hue application key (the 'username' created by " +
			"pressing the bridge's link button). Defaults to the HUE_APP_KEY environment variable of the worker."},
		{Name: "transactional", Type: arrow.FixedWidthTypes.Boolean, DefaultBatch: defBatch, Description: "Stage light changes " +
			"(UPDATE and the control functions) in DuckDB transactions and send them at COMMIT; ROLLBACK discards them. " +
			"Default false: every change is sent as its statement runs."},
	}
}

// encodeAttachOptions packs the ATTACH options batch into attach_opaque_data.
func encodeAttachOptions(optionsIPC []byte) ([]byte, BridgeOptions, error) {
	var opts BridgeOptions
	if len(optionsIPC) > 0 {
		batch, err := vgi.DeserializeRecordBatch(optionsIPC)
		if err != nil {
			return nil, opts, err
		}
		defer batch.Release()
		if batch.NumRows() > 0 {
			if c := column0(batch, "bridge"); c != nil {
				if s, ok := cellValue(c, 0).(string); ok {
					opts.Bridge = s
				}
			}
			if c := column0(batch, "app_key"); c != nil {
				if s, ok := cellValue(c, 0).(string); ok {
					opts.AppKey = s
				}
			}
			if c := column0(batch, "transactional"); c != nil {
				opts.Transactional, _ = cellValue(c, 0).(bool)
			}
		}
	}
	data, err := json.Marshal(opts)
	if err != nil {
		return nil, opts, err
	}
	return append(append([]byte(nil), attachMagic...), data...), opts, nil
}

// decodeAttachOptions unpacks attach_opaque_data; anything else means "no options".
func decodeAttachOptions(b []byte) BridgeOptions {
	var opts BridgeOptions
	if i := bytes.Index(b, attachMagic); i >= 0 {
		_ = json.Unmarshal(b[i+len(attachMagic):], &opts)
	}
	return opts
}

func attachValidator(req *vgi.CatalogAttachRequestWire, _ *vgirpc.CallContext) (*vgi.AttachDecision, error) {
	var optionsIPC []byte
	if req.Options != nil {
		optionsIPC = *req.Options
	}
	data, opts, err := encodeAttachOptions(optionsIPC)
	if err != nil {
		return nil, err
	}
	// supports_transactions per attach: only a transactional attach gets
	// BEGIN/COMMIT/ROLLBACK (the transaction hooks) and a transaction id.
	transactional := opts.Transactional
	return &vgi.AttachDecision{AttachOpaqueData: data, SupportsTransactions: &transactional}, nil
}

// callInfo is the attach options + transaction id of a process call.
type callInfo struct {
	Opts BridgeOptions
	Tx   []byte
}

func callInfoOf(p *vgi.ProcessParams) callInfo {
	return callInfo{Opts: decodeAttachOptions(p.AttachScope), Tx: p.TransactionOpaqueData}
}

func (c callInfo) connect() (BridgeConnection, error) { return resolveConnection(c.Opts) }
