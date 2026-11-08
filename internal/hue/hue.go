// A small client for the Philips Hue bridge's CLIP v2 API, plus the pure
// helpers (colour conversion, resource joining) the SQL functions share.
// Port of hue-worker/src/hue.ts.

package hue

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variables and limits.
const (
	BridgeEnvVar        = "HUE_BRIDGE"
	AppKeyEnvVar        = "HUE_APP_KEY"
	requestTimeout      = 10 * time.Second
	maxAttempts         = 4
	lightCommandGap     = 100 * time.Millisecond
	groupCommandGap     = 1000 * time.Millisecond
	maxRetryAfterSleep  = 10 * time.Second
	userAgent           = "vgi-hue-worker/" + Version
	retryBackoffBaseDur = 250 * time.Millisecond
)

// HueError is a bridge refusal or a user error, shown to DuckDB verbatim.
type HueError struct{ Msg string } //nolint:revive // name mirrors the TS worker

func (e *HueError) Error() string { return e.Msg }

func hueErr(format string, args ...any) error { return &HueError{Msg: fmt.Sprintf(format, args...)} }

// BridgeOptions are the ATTACH options (either may be absent).
type BridgeOptions struct {
	Bridge        string `json:"bridge,omitempty"`
	AppKey        string `json:"app_key,omitempty"`
	Transactional bool   `json:"transactional,omitempty"`
}

// BridgeConnection is a resolved bridge.
type BridgeConnection struct {
	BaseURL string
	AppKey  string
}

// resolveConnection combines ATTACH options with the environment.
func resolveConnection(opts BridgeOptions) (BridgeConnection, error) {
	bridge := strings.TrimSpace(firstNonEmpty(opts.Bridge, os.Getenv(BridgeEnvVar)))
	appKey := strings.TrimSpace(firstNonEmpty(opts.AppKey, os.Getenv(AppKeyEnvVar)))
	if appKey == "" {
		return BridgeConnection{}, hueErr("No Hue app key configured. Pass it when attaching: " +
			"ATTACH 'hue' (TYPE vgi, LOCATION '...', app_key getenv('HUE_APP_KEY')), " +
			"or set the " + AppKeyEnvVar + " environment variable for the worker. " +
			"The bridge address is optional: without it the bridge is discovered on your network.")
	}
	if bridge == "" || isBridgeID(bridge) {
		found, err := discoverBridge(bridge)
		if err != nil {
			return BridgeConnection{}, &HueError{Msg: err.Error()}
		}
		return BridgeConnection{BaseURL: found.URL, AppKey: appKey}, nil
	}
	if regexp.MustCompile(`(?i)^https?://`).MatchString(bridge) {
		return BridgeConnection{BaseURL: strings.TrimRight(bridge, "/"), AppKey: appKey}, nil
	}
	return BridgeConnection{BaseURL: "https://" + bridge, AppKey: appKey}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// The bridge's certificate is signed by Signify's private CA: skip verification.
var httpClient = &http.Client{
	Timeout:   requestTimeout,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // see header
}

type hueEnvelope struct {
	Data   []json.RawMessage `json:"data"`
	Errors []struct {
		Description string `json:"description"`
	} `json:"errors"`
}

// hueRequest sends one request, retrying 429/503 and network errors with backoff.
func hueRequest(conn BridgeConnection, method, path string, body any) ([]json.RawMessage, error) {
	url := conn.BaseURL + "/clip/v2" + path
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("hue-application-key", conn.AppKey)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			cancel()
			if attempt >= maxAttempts {
				return nil, hueErr("%s %s: cannot reach the Hue bridge at %s: %v", method, path, conn.BaseURL, err)
			}
			time.Sleep(retryBackoffBaseDur * time.Duration(1<<attempt))
			continue
		}
		if (resp.StatusCode == 429 || resp.StatusCode == 503) && attempt < maxAttempts {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			cancel()
			delay := retryBackoffBaseDur * time.Duration(1<<attempt)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
					delay = time.Duration(n) * time.Second
				}
			}
			logDebug("%s %s: HTTP %d, retrying in %s", method, path, resp.StatusCode, delay)
			time.Sleep(min(delay, maxRetryAfterSleep))
			continue
		}
		text, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if err != nil {
			return nil, hueErr("%s %s: reading the response: %v", method, path, err)
		}
		var env hueEnvelope
		_ = json.Unmarshal(text, &env)
		var errs []string
		for _, e := range env.Errors {
			if e.Description != "" {
				errs = append(errs, e.Description)
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			detail := strings.Join(errs, "; ")
			if len(errs) == 0 {
				detail = string(text)
				if len(detail) > 300 {
					detail = detail[:300]
				}
			}
			hint := ""
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				hint = " (is app_key correct?)"
			}
			return nil, hueErr("%s %s failed with HTTP %d%s: %s", method, path, resp.StatusCode, hint, detail)
		}
		if len(errs) > 0 {
			return nil, hueErr("%s %s: the bridge reported: %s", method, path, strings.Join(errs, "; "))
		}
		return env.Data, nil
	}
}

// ----------------------------------------------------------------- resources

type resourceRef struct {
	Rid   string `json:"rid"`
	Rtype string `json:"rtype"`
}

type metadata struct {
	Name      *string `json:"name"`
	Archetype *string `json:"archetype"`
}

type hueLight struct {
	ID       string      `json:"id"`
	Owner    resourceRef `json:"owner"`
	Metadata *metadata   `json:"metadata"`
	On       *struct {
		On bool `json:"on"`
	} `json:"on"`
	Dimming *struct {
		Brightness float64 `json:"brightness"`
	} `json:"dimming"`
	ColorTemperature *struct {
		Mirek *float64 `json:"mirek"`
	} `json:"color_temperature"`
	Color *struct {
		XY *struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		} `json:"xy"`
	} `json:"color"`
}

type hueDevice struct {
	ID          string    `json:"id"`
	Metadata    *metadata `json:"metadata"`
	ProductData *struct {
		ModelID     *string `json:"model_id"`
		ProductName *string `json:"product_name"`
	} `json:"product_data"`
}

type hueGroup struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Metadata *metadata     `json:"metadata"`
	Children []resourceRef `json:"children"`
	Services []resourceRef `json:"services"`
}

type hueGroupedLight struct {
	ID string `json:"id"`
	On *struct {
		On bool `json:"on"`
	} `json:"on"`
	Dimming *struct {
		Brightness float64 `json:"brightness"`
	} `json:"dimming"`
}

type hueScene struct {
	ID       string       `json:"id"`
	Metadata *metadata    `json:"metadata"`
	Group    *resourceRef `json:"group"`
	Status   *struct {
		Active *string `json:"active"`
	} `json:"status"`
}

type hueZigbee struct {
	ID     string      `json:"id"`
	Owner  resourceRef `json:"owner"`
	Status *string     `json:"status"`
}

type hueSensor struct {
	ID      string      `json:"id"`
	Type    string      `json:"type"`
	Owner   resourceRef `json:"owner"`
	Enabled *bool       `json:"enabled"`
	Motion  *struct {
		Motion       *bool `json:"motion"`
		MotionReport *struct {
			Changed *string `json:"changed"`
			Motion  *bool   `json:"motion"`
		} `json:"motion_report"`
	} `json:"motion"`
	Temperature *struct {
		Temperature       *float64 `json:"temperature"`
		TemperatureReport *struct {
			Changed     *string  `json:"changed"`
			Temperature *float64 `json:"temperature"`
		} `json:"temperature_report"`
	} `json:"temperature"`
	Light *struct {
		LightLevel       *float64 `json:"light_level"`
		LightLevelReport *struct {
			Changed    *string  `json:"changed"`
			LightLevel *float64 `json:"light_level"`
		} `json:"light_level_report"`
	} `json:"light"`
	ContactReport *struct {
		Changed *string `json:"changed"`
		State   *string `json:"state"`
	} `json:"contact_report"`
	PowerState *struct {
		BatteryState *string `json:"battery_state"`
		BatteryLevel *int32  `json:"battery_level"`
	} `json:"power_state"`
}

func (m *metadata) name() *string {
	if m == nil {
		return nil
	}
	return m.Name
}

func (m *metadata) archetype() *string {
	if m == nil {
		return nil
	}
	return m.Archetype
}

func nameOf(m *metadata) string {
	if n := m.name(); n != nil {
		return *n
	}
	return ""
}

func getResources[T any](conn BridgeConnection, rtype string) ([]T, error) {
	data, err := hueRequest(conn, "GET", "/resource/"+rtype, nil)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(data))
	for _, raw := range data {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, hueErr("decoding %s: %v", rtype, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// parallel runs fns concurrently and returns the first error.
func parallel(fns ...func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = fn() }()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// roomByDevice indexes rooms by the devices they contain.
func roomByDevice(groups []hueGroup) map[string]hueGroup {
	out := map[string]hueGroup{}
	for _, g := range groups {
		if g.Type != "room" {
			continue
		}
		for _, c := range g.Children {
			if c.Rtype == "device" {
				out[c.Rid] = g
			}
		}
	}
	return out
}

func reachableByDevice(zigbee []hueZigbee) map[string]bool {
	out := map[string]bool{}
	for _, z := range zigbee {
		out[z.Owner.Rid] = z.Status != nil && *z.Status == "connected"
	}
	return out
}

// lightsInGroup lists the lights a room (devices) or zone (lights) covers.
func lightsInGroup(g hueGroup, lights []hueLight) []hueLight {
	devices, ids := map[string]bool{}, map[string]bool{}
	for _, c := range g.Children {
		switch c.Rtype {
		case "device":
			devices[c.Rid] = true
		case "light":
			ids[c.Rid] = true
		}
	}
	var out []hueLight
	for _, l := range lights {
		if ids[l.ID] || devices[l.Owner.Rid] {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------- colour math

func jsRound(v float64) float64 { return math.Floor(v + 0.5) }

func kelvinToMirek(kelvin float64) int64 {
	return int64(math.Min(500, math.Max(153, jsRound(1_000_000/kelvin))))
}

func mirekToKelvin(mirek float64) int32 { return int32(jsRound(1_000_000 / mirek)) }

var namedColors = map[string]string{
	"red": "#ff0000", "orange": "#ff8000", "yellow": "#ffff00", "green": "#00ff00", "cyan": "#00ffff",
	"blue": "#0000ff", "purple": "#8000ff", "magenta": "#ff00ff", "pink": "#ff69b4", "white": "#ffffff",
}

var namedColorOrder = []string{"red", "orange", "yellow", "green", "cyan", "blue", "purple", "magenta", "pink", "white"}

var (
	hex3 = regexp.MustCompile(`(?i)^[0-9a-f]{3}$`)
	hex6 = regexp.MustCompile(`(?i)^[0-9a-f]{6}$`)
)

func parseColor(color string) ([3]float64, error) {
	raw := strings.TrimSpace(color)
	if v, ok := namedColors[strings.ToLower(raw)]; ok {
		raw = v
	}
	h := strings.TrimPrefix(raw, "#")
	if hex3.MatchString(h) {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if !hex6.MatchString(h) {
		return [3]float64{}, hueErr("invalid color '%s': use '#rrggbb' or one of %s", color, strings.Join(namedColorOrder, ", "))
	}
	var rgb [3]float64
	for i := range 3 {
		v, _ := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		rgb[i] = float64(v)
	}
	return rgb, nil
}

func gammaExpand(c float64) float64 {
	if c > 0.04045 {
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return c / 12.92
}

func gammaCompress(c float64) float64 {
	if c <= 0.0031308 {
		return 12.92 * c
	}
	return 1.055*math.Pow(c, 1/2.4) - 0.055
}

func round4(v float64) float64 { return jsRound(v*10_000) / 10_000 }

func rgbToXY(rgb [3]float64) (float64, float64) {
	r, g, b := gammaExpand(rgb[0]/255), gammaExpand(rgb[1]/255), gammaExpand(rgb[2]/255)
	X := r*0.664511 + g*0.154324 + b*0.162028
	Y := r*0.283881 + g*0.668433 + b*0.047685
	Z := r*0.000088 + g*0.07231 + b*0.986039
	sum := X + Y + Z
	if sum == 0 {
		return 0.3127, 0.329
	}
	return round4(X / sum), round4(Y / sum)
}

func toHex(rgb [3]float64) string {
	var sb strings.Builder
	sb.WriteByte('#')
	for _, c := range rgb {
		fmt.Fprintf(&sb, "%02x", int(jsRound(math.Min(255, math.Max(0, c)))))
	}
	return sb.String()
}

func xyToHex(x, y float64) string {
	if y <= 0 {
		return "#000000"
	}
	Y := 1.0
	X := (Y / y) * x
	Z := (Y / y) * (1 - x - y)
	rgb := [3]float64{
		X*1.656492 - Y*0.354851 - Z*0.255038,
		-X*0.707196 + Y*1.655397 + Z*0.036152,
		X*0.051713 - Y*0.121364 + Z*1.01153,
	}
	mx := 1e-9
	for i := range rgb {
		rgb[i] = math.Max(0, rgb[i])
		mx = math.Max(mx, rgb[i])
	}
	for i := range rgb {
		rgb[i] = gammaCompress(rgb[i]/mx) * 255
	}
	return toHex(rgb)
}

func kelvinToHex(kelvin float64) string {
	t := kelvin / 100
	var r, g, b float64
	if t <= 66 {
		r = 255
		g = 99.4708025861*math.Log(t) - 161.1195681661
	} else {
		r = 329.698727446 * math.Pow(t-60, -0.1332047592)
		g = 288.1221695283 * math.Pow(t-60, -0.0755148492)
	}
	switch {
	case t >= 66:
		b = 255
	case t <= 19:
		b = 0
	default:
		b = 138.5177312231*math.Log(t-10) - 305.0447927307
	}
	return toHex([3]float64{r, g, b})
}

func lightLevelToLux(level float64) float64 {
	return jsRound(math.Pow(10, (level-1)/10_000)*100) / 100
}

// ------------------------------------------------------------------- commands

// lightChange is what a set_* call asks for; nil fields are left alone.
type lightChange struct {
	On           *bool
	Brightness   *float64
	Kelvin       *float64
	Color        *string
	TransitionMs *float64
}

// buildStateBody builds the CLIP v2 PUT body (key order mirrors the TS worker).
func buildStateBody(c lightChange) (omap, error) {
	body := omap{}
	if c.Kelvin != nil && c.Color != nil {
		return nil, hueErr("pass either kelvin or color, not both")
	}
	if c.Brightness != nil {
		b := *c.Brightness
		if b < 0 || b > 100 {
			return nil, hueErr("brightness must be 0–100, got %s", jsNum(b))
		}
		if b == 0 {
			if c.On != nil && *c.On {
				return nil, hueErr("brightness 0 turns the light off; it cannot be combined with power := true")
			}
			body = body.set("on", omap{{"on", false}})
		} else {
			body = body.set("dimming", omap{{"brightness", b}})
			if c.On == nil {
				body = body.set("on", omap{{"on", true}})
			}
		}
	}
	if c.On != nil {
		body = body.set("on", omap{{"on", *c.On}})
	}
	if c.Kelvin != nil {
		k := *c.Kelvin
		if k < 2000 || k > 6500 {
			return nil, hueErr("kelvin must be 2000–6500, got %s", jsNum(k))
		}
		body = body.set("color_temperature", omap{{"mirek", kelvinToMirek(k)}})
	}
	if c.Color != nil {
		rgb, err := parseColor(*c.Color)
		if err != nil {
			return nil, err
		}
		x, y := rgbToXY(rgb)
		body = body.set("color", omap{{"xy", omap{{"x", x}, {"y", y}}}})
	}
	if c.TransitionMs != nil {
		if *c.TransitionMs < 0 {
			return nil, hueErr("transition_ms must be >= 0, got %s", jsNum(*c.TransitionMs))
		}
		body = body.set("dynamics", omap{{"duration", int64(jsRound(*c.TransitionMs))}})
	}
	if len(body) == 0 || (len(body) == 1 && body.has("dynamics")) {
		return nil, hueErr("nothing to change: pass at least one of power, brightness, kelvin or color")
	}
	return body, nil
}

func jsNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

var (
	paceMu        sync.Mutex
	lastCommandAt = map[string]time.Time{}
)

// pace spaces commands of one kind so a bulk update stays inside Hue's limits.
func pace(kind string) {
	gap := lightCommandGap
	if kind != "light" {
		kind, gap = "group", groupCommandGap
	}
	paceMu.Lock()
	defer paceMu.Unlock()
	if wait := time.Until(lastCommandAt[kind].Add(gap)); wait > 0 {
		time.Sleep(wait)
	}
	lastCommandAt[kind] = time.Now()
}

func sortedNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func logDebug(format string, args ...any) {
	dst := os.Getenv("HUE_DEBUG")
	if dst == "" {
		return
	}
	if strings.HasPrefix(dst, "/") { // a log file (DuckDB hides worker stderr)
		if f, err := os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = fmt.Fprintf(f, "hue: "+format+"\n", args...)
			_ = f.Close()
		}
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "hue: "+format+"\n", args...)
}
