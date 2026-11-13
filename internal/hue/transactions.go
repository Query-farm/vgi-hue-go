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

// txSchema is the staged-request store's schema.
const txSchema = `CREATE TABLE IF NOT EXISTS staged (seq INTEGER PRIMARY KEY AUTOINCREMENT,
	tx TEXT NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, body TEXT NOT NULL, created REAL NOT NULL);
	CREATE INDEX IF NOT EXISTS staged_tx ON staged (tx, seq)`

// txDir is where the staged-request store lives: HUE_TX_DIR, else a per-user
// directory (the user cache dir: %LocalAppData% on Windows), so users never
// share one file and every process of one user resolves the same path.
func txDir() string {
	if dir := os.Getenv("HUE_TX_DIR"); dir != "" {
		return dir
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "vgi-hue")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("vgi-hue-%d", os.Getuid()))
}

func openTxDB() (*sql.DB, error) {
	txDBOnce.Do(func() {
		dir := txDir()
		if txDBErr = os.MkdirAll(dir, 0o700); txDBErr != nil {
			return
		}
		txDB, txDBErr = openSharedSQLite(filepath.Join(dir, "transactions-go.db"), txSchema)
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
			// A later colour, white or gradient replaces an earlier one.
			if r.Body.has("color") {
				merged = merged.without("color_temperature", "gradient")
			}
			if r.Body.has("color_temperature") {
				merged = merged.without("color", "gradient")
			}
			if r.Body.has("gradient") {
				merged = merged.without("color", "color_temperature")
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
		body = body.set("color", xyBody(*l.Color.XY))
	}
	if g := l.Gradient; g != nil && len(g.Points) > 0 {
		mode := ""
		if g.Mode != nil {
			mode = *g.Mode
		}
		body = body.set("gradient", gradientBody(g.xys(), mode))
	}
	if e := l.Effects; e != nil && e.Status != nil && *e.Status != "" {
		body = body.set("effects", omap{{"effect", *e.Status}})
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
	sent, err := sendAll(conn, reqs)
	if err != nil {
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
			opts := optionsOf(tx.AttachOptions())
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
		_, err := sendAll(conn, reqs)
		return false, err
	}
}

// sendAll PUTs reqs to the bridge, pipelined, and returns how many succeeded.
//
//   - Starts are paced: each request starts on its pace slot (lightCommandGap
//     for lights, groupCommandGap for grouped_light/scene), without waiting
//     for earlier responses, and at most maxInFlightRequests run at once.
//   - Requests run in order per target: a target's next request starts only
//     after its previous one completed. A grouped_light or scene request
//     covers many lights, so it is a barrier: it starts after everything
//     before it completed, and nothing after it starts until it completed.
//   - hueRequest retries 429/503/network errors per request, as before.
//   - On the first error no new request starts; sendAll waits for the ones in
//     flight and returns that error. Requests already sent stay applied.
func sendAll(conn BridgeConnection, reqs []stagedRequest) (int, error) {
	var (
		mu       sync.Mutex
		firstErr error
		ok       int
		wg       sync.WaitGroup
	)
	failed := func() bool { mu.Lock(); defer mu.Unlock(); return firstErr != nil }
	slots := make(chan struct{}, maxInFlightRequests)
	last := map[string]chan struct{}{} // target -> closed once its latest request completed
	var started []chan struct{}        // every request started since the last barrier
	var barrier chan struct{}          // the latest grouped_light/scene request
	waitAll := func(chs []chan struct{}) {
		for _, ch := range chs {
			<-ch
		}
	}
	for _, r := range reqs {
		isBarrier := r.Kind != "light"
		key := r.Kind + "/" + r.Target
		if barrier != nil {
			<-barrier
		}
		if isBarrier {
			waitAll(started)
		} else if prev := last[key]; prev != nil {
			<-prev
		}
		if failed() {
			break
		}
		slots <- struct{}{}
		pace(r.Kind)
		if failed() {
			<-slots
			break
		}
		done := make(chan struct{})
		last[key] = done
		if isBarrier {
			barrier, started = done, nil
		} else {
			started = append(started, done)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			_, err := hueRequest(conn, "PUT", r.path(), r.Body)
			// Record the outcome before freeing the slot, so the loop sees a
			// failure before it can start another request.
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			} else if err == nil {
				ok++
			}
			mu.Unlock()
			<-slots
		}()
	}
	wg.Wait()
	return ok, firstErr
}

func senderFor(ci callInfo, conn BridgeConnection) sender {
	tx := stageable(ci.Opts.Transactional, ci.Tx)
	if tx == nil {
		return immediate(conn)
	}
	return func(reqs []stagedRequest) (bool, error) { return true, stage(tx, reqs) }
}
