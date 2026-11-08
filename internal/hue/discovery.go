// Find a Hue bridge when no `bridge` address was given: cached bridge, mDNS,
// then discovery.meethue.com. Port of hue-worker/src/discovery.ts.

package hue

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDiscoveryURL = "https://discovery.meethue.com"
	mdnsWindow          = 1500 * time.Millisecond
	probeTimeout        = 3 * time.Second
	cloudTimeout        = 5 * time.Second
	cacheTTL            = 7 * 24 * time.Hour
)

type discoveredBridge struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
	URL  string  `json:"url"`
}

type candidate struct {
	Host string
	Port int
}

var bridgeIDRe = regexp.MustCompile(`(?i)^[0-9a-f]{16}$`)

func isBridgeID(v string) bool { return bridgeIDRe.MatchString(v) }

func candidateURL(c candidate, scheme string) string {
	host := c.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	def := 443
	if scheme != "https" {
		def = 80
	}
	if c.Port != 0 && c.Port != def {
		return fmt.Sprintf("%s://%s:%d", scheme, host, c.Port)
	}
	return scheme + "://" + host
}

var probeClient = &http.Client{Timeout: probeTimeout, Transport: httpClient.Transport}

func probeBridge(u string) (string, *string, bool) {
	resp, err := probeClient.Get(u + "/api/config")
	if err != nil {
		return "", nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return "", nil, false
	}
	var body struct {
		BridgeID string  `json:"bridgeid"`
		Name     *string `json:"name"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.BridgeID == "" {
		return "", nil, false
	}
	return strings.ToLower(body.BridgeID), body.Name, true
}

func cloudCandidates(u string) []candidate {
	c := &http.Client{Timeout: cloudTimeout}
	resp, err := c.Get(u)
	if err != nil {
		logDebug("cloud discovery failed: %v", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil
	}
	var body []struct {
		ID                *string `json:"id"`
		InternalIPAddress *string `json:"internalipaddress"`
		Port              *int    `json:"port"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	var out []candidate
	for _, b := range body {
		if b.InternalIPAddress == nil {
			continue
		}
		c := candidate{Host: *b.InternalIPAddress}
		if b.Port != nil {
			c.Port = *b.Port
		}
		out = append(out, c)
	}
	return out
}

// mdnsQuery asks for PTR records of _hue._tcp.local.
func mdnsQuery() []byte {
	q := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range []string{"_hue", "_tcp", "local"} {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	return append(q, 0, 0, 12, 0, 1)
}

func mdnsCandidates() []candidate {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.WriteToUDP(mdnsQuery(), &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(mdnsWindow))
	hosts := map[string]bool{}
	buf := make([]byte, 9000)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n >= 12 && buf[0] == 0 && buf[1] == 0 && buf[2]&0x80 != 0 {
			hosts[addr.IP.String()] = true
		}
	}
	var out []candidate
	for h := range hosts {
		out = append(out, candidate{Host: h})
	}
	return out
}

func cacheFile() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "vgi-hue", "bridges.json")
}

type cacheEntry struct {
	SavedAt int64              `json:"savedAt"`
	Bridges []discoveredBridge `json:"bridges"`
}

func readCache(key string) []discoveredBridge {
	data, err := os.ReadFile(cacheFile())
	if err != nil {
		return nil
	}
	all := map[string]cacheEntry{}
	if json.Unmarshal(data, &all) != nil {
		return nil
	}
	e, ok := all[key]
	if !ok || time.Since(time.UnixMilli(e.SavedAt)) > cacheTTL {
		return nil
	}
	return e.Bridges
}

func writeCache(key string, bridges []discoveredBridge) {
	f := cacheFile()
	all := map[string]cacheEntry{}
	if data, err := os.ReadFile(f); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	all[key] = cacheEntry{SavedAt: time.Now().UnixMilli(), Bridges: bridges}
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	data, _ := json.MarshalIndent(all, "", "  ")
	_ = os.WriteFile(f, data, 0o600)
}

func confirm(cands []candidate, scheme string) []discoveredBridge {
	seen := map[string]bool{}
	var urls []string
	for _, c := range cands {
		u := candidateURL(c, scheme)
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	type res struct {
		id   string
		name *string
		ok   bool
	}
	results := make([]res, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() { defer wg.Done(); id, n, ok := probeBridge(u); results[i] = res{id, n, ok} }()
	}
	wg.Wait()
	var out []discoveredBridge
	ids := map[string]bool{}
	for i, r := range results {
		if r.ok && !ids[r.id] {
			ids[r.id] = true
			out = append(out, discoveredBridge{ID: r.id, Name: r.name, URL: urls[i]})
		}
	}
	return out
}

func chooseBridge(bridges []discoveredBridge, wantedID string) (discoveredBridge, error) {
	var pool []discoveredBridge
	var list []string
	for _, b := range bridges {
		if wantedID == "" || b.ID == strings.ToLower(wantedID) {
			pool = append(pool, b)
		}
		s := b.ID + " at " + b.URL
		if b.Name != nil && *b.Name != "" {
			s += " ('" + *b.Name + "')"
		}
		list = append(list, s)
	}
	if len(pool) == 1 {
		return pool[0], nil
	}
	if len(pool) == 0 {
		if wantedID != "" {
			found := ""
			if len(bridges) > 0 {
				found = " (found: " + strings.Join(list, ", ") + ")"
			}
			return discoveredBridge{}, hueErr("no Hue bridge with id %s found on the network%s", wantedID, found)
		}
		return discoveredBridge{}, hueErr("no Hue bridge found on the network (tried mDNS and https://discovery.meethue.com). " +
			"Pass its address: ATTACH ... (bridge '192.168.1.20', ...), or set HUE_BRIDGE.")
	}
	return discoveredBridge{}, hueErr("found %d Hue bridges: %s. Pick one with bridge '<id or address>' in ATTACH or HUE_BRIDGE.",
		len(pool), strings.Join(list, ", "))
}

var (
	discoveryMu sync.Mutex
	memoryCache = map[string][]discoveredBridge{}
)

func discoverBridge(wantedID string) (discoveredBridge, error) {
	if strings.EqualFold(os.Getenv("HUE_DISCOVERY"), "off") {
		return discoveredBridge{}, hueErr("no Hue bridge address given and discovery is off (HUE_DISCOVERY=off). " +
			"Pass bridge '<address>' in ATTACH or set HUE_BRIDGE.")
	}
	scheme := firstNonEmpty(os.Getenv("HUE_DISCOVERY_SCHEME"), "https")
	cloudURL := firstNonEmpty(os.Getenv("HUE_DISCOVERY_URL"), defaultDiscoveryURL)
	mdnsOn := !strings.EqualFold(os.Getenv("HUE_DISCOVERY_MDNS"), "off")
	key := scheme + "|" + cloudURL + "|"
	if mdnsOn {
		key += "mdns"
	} else {
		key += "nomdns"
	}
	fits := func(bs []discoveredBridge) bool {
		if wantedID != "" {
			for _, b := range bs {
				if b.ID == strings.ToLower(wantedID) {
					return true
				}
			}
			return false
		}
		return len(bs) == 1
	}
	discoveryMu.Lock()
	remembered := memoryCache[key]
	discoveryMu.Unlock()
	if len(remembered) == 0 {
		remembered = readCache(key)
	}
	if len(remembered) > 0 {
		var cands []candidate
		for _, b := range remembered {
			if u, err := url.Parse(b.URL); err == nil {
				p, _ := strconv.Atoi(u.Port())
				cands = append(cands, candidate{Host: u.Hostname(), Port: p})
			}
		}
		if still := confirm(cands, scheme); fits(still) {
			discoveryMu.Lock()
			memoryCache[key] = still
			discoveryMu.Unlock()
			return chooseBridge(still, wantedID)
		}
	}
	var found []discoveredBridge
	if mdnsOn {
		found = confirm(mdnsCandidates(), scheme)
	}
	if !fits(found) {
		ids := map[string]bool{}
		for _, b := range found {
			ids[b.ID] = true
		}
		for _, b := range confirm(cloudCandidates(cloudURL), scheme) {
			if !ids[b.ID] {
				found = append(found, b)
			}
		}
	}
	if len(found) > 0 {
		discoveryMu.Lock()
		memoryCache[key] = found
		discoveryMu.Unlock()
		writeCache(key, found)
	}
	return chooseBridge(found, wantedID)
}
