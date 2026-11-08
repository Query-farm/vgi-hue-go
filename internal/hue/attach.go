// ATTACH options: bridge, app_key (secret), transactional.
//
// Functions and the transaction hooks read them with the SDK's AttachOptions()
// accessor (ProcessParams / TransactionHookContext). The SDK keeps the options
// in the attach value, with app_key encrypted under a key the client never has.

package hue

import (
	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

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

// optionsOf reads the ATTACH options through the SDK's accessor. app_key is
// declared Secret: the SDK carries it encrypted inside the attach value and
// decrypts it only here, so it never appears in attach bytes, bind opaque data
// or function state.
func optionsOf(o *vgi.AttachOptions) BridgeOptions {
	var opts BridgeOptions
	opts.Bridge, _ = o.String("bridge")
	opts.AppKey, _ = o.String("app_key")
	opts.Transactional, _ = o.Bool("transactional")
	return opts
}

// attachValidator decides supports_transactions per attach from the
// transactional option; it stores nothing in the attach value.
func attachValidator(req *vgi.CatalogAttachRequestWire, _ *vgirpc.CallContext) (*vgi.AttachDecision, error) {
	transactional := false
	if req.Options != nil && len(*req.Options) > 0 {
		batch, err := vgi.DeserializeRecordBatch(*req.Options)
		if err != nil {
			return nil, err
		}
		defer batch.Release()
		if c := column0(batch, "transactional"); c != nil && batch.NumRows() > 0 {
			transactional, _ = cellValue(c, 0).(bool)
		}
	}
	// Only a transactional attach gets BEGIN/COMMIT/ROLLBACK (the
	// transaction hooks) and a transaction id.
	return &vgi.AttachDecision{SupportsTransactions: &transactional}, nil
}

// callInfo is the attach options + transaction id of a process call.
type callInfo struct {
	Opts BridgeOptions
	Tx   []byte
}

func callInfoOf(p *vgi.ProcessParams) callInfo {
	return callInfo{Opts: optionsOf(p.AttachOptions()), Tx: p.TransactionOpaqueData}
}

func (c callInfo) connect() (BridgeConnection, error) { return resolveConnection(c.Opts) }
