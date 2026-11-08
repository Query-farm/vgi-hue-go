// Transactional light changes: ATTACH ... (transactional true).
//
// In a transactional attach UPDATE and the control functions stage their
// requests under the transaction id DuckDB passes on every call; COMMIT
// sends them and ROLLBACK discards them (vgi.WithTransactionHooks). Port of
// transactions.ts. Only a transactional attach reports supports_transactions,
// so only it gets the hooks and a transaction id.

package hue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	_ "modernc.org/sqlite" // staged-request store
)

const staleAfter = 24 * time.Hour

// stagedRequest is one PUT a transaction will send.
type stagedRequest struct {
	Kind   string // light | grouped_light | scene
	Target string
	Body   omap
}

func (r stagedRequest) path() string {
	return "/resource/" + r.Kind + "/" + url.PathEscape(r.Target)
}

var (
	txDBOnce sync.Once
	txDB     *sql.DB
	txDBErr  error
)

func openTxDB() (*sql.DB, error) {
	txDBOnce.Do(func() {
		dir := os.Getenv("HUE_TX_DIR")
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "vgi-hue")
		}
		if txDBErr = os.MkdirAll(dir, 0o755); txDBErr != nil {
			return
		}
		txDB, txDBErr = sql.Open("sqlite", "file:"+filepath.Join(dir, "transactions-go.db")+
			"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if txDBErr != nil {
			return
		}
		_, txDBErr = txDB.Exec(`CREATE TABLE IF NOT EXISTS staged (seq INTEGER PRIMARY KEY AUTOINCREMENT,
			tx TEXT NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, body TEXT NOT NULL, created REAL NOT NULL);
			CREATE INDEX IF NOT EXISTS staged_tx ON staged (tx, seq)`)
	})
	return txDB, txDBErr
}

// stageable returns the transaction id to stage under, or nil to act now.
func stageable(transactional bool, tx []byte) []byte {
	if !transactional || len(tx) == 0 {
		return nil
	}
	return tx
}

func stage(tx []byte, reqs []stagedRequest) error {
	if len(reqs) == 0 {
		return nil
	}
	db, err := openTxDB()
	if err != nil {
		return err
	}
	t, err := db.Begin()
	if err != nil {
		return err
	}
	now := float64(time.Now().UnixMilli())
	for _, r := range reqs {
		body, _ := json.Marshal(r.Body)
		if _, err := t.Exec("INSERT INTO staged (tx, kind, target, body, created) VALUES (?, ?, ?, ?, ?)",
			hex.EncodeToString(tx), r.Kind, r.Target, string(body), now); err != nil {
			_ = t.Rollback()
			return err
		}
	}
	return t.Commit()
}

func staged(tx []byte) ([]stagedRequest, error) {
	db, err := openTxDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT kind, target, body FROM staged WHERE tx = ? ORDER BY seq", hex.EncodeToString(tx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []stagedRequest
	for rows.Next() {
		var r stagedRequest
		var body string
		if err := rows.Scan(&r.Kind, &r.Target, &body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &r.Body); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func discard(tx []byte) error {
	db, err := openTxDB()
	if err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM staged WHERE tx = ?", hex.EncodeToString(tx))
	return err
}

// pendingLightBodies groups staged light-level bodies by light id.
func pendingLightBodies(tx []byte) (map[string][]omap, error) {
	reqs, err := staged(tx)
	if err != nil {
		return nil, err
	}
	out := map[string][]omap{}
	for _, r := range reqs {
		if r.Kind == "light" {
			out[r.Target] = append(out[r.Target], r.Body)
		}
	}
	return out, nil
}

// coalesce merges back-to-back state changes to the same light.
func coalesce(reqs []stagedRequest) []stagedRequest {
	var out []stagedRequest
	for _, r := range reqs {
		isState := !r.Body.has("metadata")
		if n := len(out); n > 0 && isState && out[n-1].Kind == "light" && r.Kind == "light" &&
			out[n-1].Target == r.Target && !out[n-1].Body.has("metadata") {
			merged := append(omap{}, out[n-1].Body...)
			for _, e := range r.Body {
				merged = merged.set(e.K, e.V)
			}
			if r.Body.has("color") {
				merged = merged.without("color_temperature")
			}
			if r.Body.has("color_temperature") {
				merged = merged.without("color")
			}
			out[n-1].Body = merged
		} else {
			out = append(out, r)
		}
	}
	return out
}

func restoreBody(l hueLight) omap {
	on := l.On != nil && l.On.On
	body := omap{{"on", omap{{"on", on}}}}
	if l.Dimming != nil {
		body = body.set("dimming", omap{{"brightness", l.Dimming.Brightness}})
	}
	if l.ColorTemperature != nil && l.ColorTemperature.Mirek != nil {
		body = body.set("color_temperature", omap{{"mirek", *l.ColorTemperature.Mirek}})
	} else if l.Color != nil && l.Color.XY != nil {
		body = body.set("color", omap{{"xy", omap{{"x", l.Color.XY.X}, {"y", l.Color.XY.Y}}}})
	}
	return body
}

func touchedLights(conn BridgeConnection, reqs []stagedRequest, lights []hueLight) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	nonLight := false
	for _, r := range reqs {
		if r.Kind == "light" {
			add(r.Target)
		} else {
			nonLight = true
		}
	}
	if !nonLight {
		return ids, nil
	}
	var rooms, zones []hueGroup
	var scenes []hueScene
	if err := parallel(
		func() (e error) { rooms, e = getResources[hueGroup](conn, "room"); return },
		func() (e error) { zones, e = getResources[hueGroup](conn, "zone"); return },
		func() (e error) { scenes, e = getResources[hueScene](conn, "scene"); return },
	); err != nil {
		return nil, err
	}
	groups := append(rooms, zones...)
	for _, r := range reqs {
		var group *hueGroup
		for i := range groups {
			g := &groups[i]
			switch r.Kind {
			case "grouped_light":
				for _, s := range g.Services {
					if s.Rid == r.Target {
						group = g
					}
				}
			case "scene":
				for _, s := range scenes {
					if s.ID == r.Target && s.Group != nil && s.Group.Rid == g.ID {
						group = g
					}
				}
			}
		}
		if group != nil {
			for _, l := range lightsInGroup(*group, lights) {
				add(l.ID)
			}
		}
	}
	return ids, nil
}

// commitTransaction sends a transaction's staged requests; on a refusal it
// restores the lights already changed and fails.
func commitTransaction(opts BridgeOptions, tx []byte) error {
	all, err := staged(tx)
	if err != nil {
		return err
	}
	reqs := coalesce(all)
	if err := discard(tx); err != nil {
		return err
	}
	if len(reqs) == 0 {
		return nil
	}
	conn, err := resolveConnection(opts)
	if err != nil {
		return err
	}
	lights, err := getResources[hueLight](conn, "light")
	if err != nil {
		return err
	}
	before := map[string]hueLight{}
	for _, l := range lights {
		before[l.ID] = l
	}
	touched, err := touchedLights(conn, reqs, lights)
	if err != nil {
		return err
	}
	sent := 0
	for _, r := range reqs {
		pace(r.Kind)
		if _, err := hueRequest(conn, "PUT", r.path(), r.Body); err != nil {
			restored := 0
			if sent > 0 {
				restored = restoreLights(conn, touched, before)
			}
			msg := fmt.Sprintf("COMMIT failed after %d of %d light changes: %s. ", sent, len(reqs), err.Error())
			if sent > 0 {
				msg += fmt.Sprintf("Restored %d light(s) to their state before the commit.", restored)
			} else {
				msg += "Nothing was changed."
			}
			return &HueError{Msg: msg}
		}
		sent++
	}
	return nil
}

func restoreLights(conn BridgeConnection, ids []string, before map[string]hueLight) int {
	restored := 0
	for _, id := range ids {
		l, ok := before[id]
		if !ok {
			continue
		}
		pace("light")
		if _, err := hueRequest(conn, "PUT", "/resource/light/"+id, restoreBody(l)); err != nil {
			fmt.Fprintf(os.Stderr, "hue: could not restore light %s after a failed commit: %v\n", id, err)
			continue
		}
		restored++
	}
	return restored
}

func rollbackTransaction(tx []byte) error { return discard(tx) }

// sweepStale drops requests of transactions abandoned without COMMIT/ROLLBACK
// (a worker that died mid-transaction).
func sweepStale() error {
	db, err := openTxDB()
	if err != nil {
		return err
	}
	cutoff := float64(time.Now().UnixMilli()) - float64(staleAfter.Milliseconds())
	_, err = db.Exec("DELETE FROM staged WHERE created < ?", cutoff)
	return err
}

// transactionHooks: begin mints the id (and sweeps abandoned transactions),
// COMMIT sends the staged requests (a refusal fails COMMIT with its message),
// ROLLBACK discards them.
func transactionHooks() vgi.TransactionHooks {
	return vgi.TransactionHooks{
		Begin: func(_ context.Context, _ vgi.TransactionHookContext) ([]byte, error) {
			if err := sweepStale(); err != nil {
				return nil, err
			}
			id := make([]byte, 16)
			_, err := rand.Read(id)
			return id, err
		},
		Commit: func(_ context.Context, tx vgi.TransactionHookContext) error {
			logDebug("commit tx=%x", tx.TransactionOpaqueData)
			opts := decodeAttachOptions(tx.AttachOpaqueData)
			if !opts.Transactional {
				return nil
			}
			return commitTransaction(opts, tx.TransactionOpaqueData)
		},
		Rollback: func(_ context.Context, tx vgi.TransactionHookContext) error {
			return rollbackTransaction(tx.TransactionOpaqueData)
		},
	}
}

// ------------------------------------------------------------------ dispatch

// sender sends (or stages) requests and reports which happened.
type sender func(reqs []stagedRequest) (staged bool, err error)

func immediate(conn BridgeConnection) sender {
	return func(reqs []stagedRequest) (bool, error) {
		for _, r := range reqs {
			pace(r.Kind)
			if _, err := hueRequest(conn, "PUT", r.path(), r.Body); err != nil {
				return false, err
			}
		}
		return false, nil
	}
}

func senderFor(ci callInfo, conn BridgeConnection) sender {
	tx := stageable(ci.Opts.Transactional, ci.Tx)
	if tx == nil {
		return immediate(conn)
	}
	return func(reqs []stagedRequest) (bool, error) { return true, stage(tx, reqs) }
}
