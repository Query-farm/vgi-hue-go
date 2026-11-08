package hue

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestGreeting(t *testing.T) {
	if got := Greeting("Ada", "casual"); got != "Hi, Ada!" {
		t.Errorf("casual = %q", got)
	}
	if got := Greeting("Ada", "formal"); got != "Good day, Ada." {
		t.Errorf("formal = %q", got)
	}
}

func TestGreetColumnKeepsNulls(t *testing.T) {
	b := array.NewStringBuilder(memory.NewGoAllocator())
	b.AppendValues([]string{"Ada", "", "Grace"}, []bool{true, false, true})
	col := b.NewArray()
	defer col.Release()
	in := array.NewRecordBatch(arrow.NewSchema([]arrow.Field{{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true}}, nil),
		[]arrow.Array{col}, 3)
	defer in.Release()
	params := &vgi.ProcessParams{OutputSchema: arrow.NewSchema(
		[]arrow.Field{{Name: "result", Type: arrow.BinaryTypes.String, Nullable: true}}, nil)}

	out, err := (&GreetWithStyle{}).ProcessTyped(context.Background(), &greetWithStyleArgs{Style: "formal"}, params, in)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	got := out.Column(0).(*array.String)
	if got.Value(0) != "Good day, Ada." || !got.IsNull(1) || got.Value(2) != "Good day, Grace." {
		t.Errorf("unexpected column %v", got)
	}
}

// The constraints in the struct tags are what DuckDB enforces at bind time.
func TestArgumentConstraints(t *testing.T) {
	specs := (&SequenceTable{}).ArgumentSpecs()
	if len(specs) != 2 || specs[0].Name != "count" || specs[0].Position != 0 || *specs[0].Ge != 0 || *specs[0].Le != 1_000_000 {
		t.Fatalf("count spec: %+v", specs[0])
	}
	if specs[1].Name != "step" || specs[1].Position != -1 || specs[1].DefaultValue != "1" || *specs[1].Ge != 1 {
		t.Fatalf("step spec: %+v", specs[1])
	}
	fetch := (&FetchItems{}).ArgumentSpecs()
	if fetch[0].Pattern != "^https?://" || fetch[1].Name != "page_size" || *fetch[1].Le != 1000 {
		t.Fatalf("fetch specs: %+v", fetch)
	}
	style := vgi.DeriveArgSpecs(greetWithStyleArgs{})[1]
	if len(style.Choices) != 2 || style.Doc == "" {
		t.Fatalf("style spec: %+v", style)
	}
}

// Over HTTP the SDK gob-encodes state between batches. A cursor that does not
// survive the round trip restarts the scan forever, so check it here.
func TestStateSurvivesGobRoundTrip(t *testing.T) {
	roundTrip := func(in, out any) {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(in); err != nil {
			t.Fatal(err)
		}
		if err := gob.NewDecoder(&buf).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	seq := SequenceState{Emitted: 20_000, Count: 25_000, Step: 3}
	var seq2 SequenceState
	roundTrip(seq, &seq2)
	if seq2 != seq {
		t.Errorf("sequence state lost: %+v", seq2)
	}
	fetch := FetchState{NextPage: 4, BaseURL: "http://x", PageSize: 10}
	var fetch2 FetchState
	roundTrip(fetch, &fetch2)
	if fetch2 != fetch {
		t.Errorf("fetch state lost: %+v", fetch2)
	}
}

func TestPageURLAndParse(t *testing.T) {
	if got := pageURL("http://h:1/", 2, 50); got != "http://h:1/items?page=2&page_size=50" {
		t.Errorf("pageURL = %q", got)
	}
	items, next, err := parsePage([]byte(`{"items":[{"id":1,"name":"a","category":null,"price":1.5}],"next_page":null}`))
	if err != nil || len(items) != 1 || next != 0 || items[0].Category != nil || *items[0].Price != 1.5 {
		t.Errorf("parsePage = %+v %d %v", items, next, err)
	}
}

func TestTokenPrefersSecret(t *testing.T) {
	t.Setenv(TokenEnvVar, "from-env")
	if got := token(nil); got != "from-env" {
		t.Errorf("env token = %q", got)
	}
	secrets := vgi.Secrets{"s": {"type": SecretType, "token": "from-secret"}}
	if got := token(secrets); got != "from-secret" {
		t.Errorf("secret token = %q", got)
	}
}

func TestGetJSONRetriesAndReportsStatus(t *testing.T) {
	sleep = func(context.Context, time.Duration) {}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/denied":
			http.Error(w, "nope", http.StatusUnauthorized)
		case calls.Add(1) == 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			if r.Header.Get("Authorization") != "Bearer t" {
				t.Errorf("missing bearer token")
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer srv.Close()

	body, err := getJSON(context.Background(), srv.URL+"/items", "t")
	if err != nil || string(body) != `{"items":[]}` || calls.Load() != 2 {
		t.Fatalf("getJSON = %q, %v after %d calls", body, err, calls.Load())
	}
	_, err = getJSON(context.Background(), srv.URL+"/denied", "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("want an HTTP 401 error, got %v", err)
	}
}

func TestRetryDelay(t *testing.T) {
	if retryDelay("2", 1) != 2*time.Second || retryDelay("999", 1) != 30*time.Second {
		t.Error("Retry-After not honoured")
	}
	if retryDelay("", 1) != time.Second || retryDelay("soon", 2) != 2*time.Second {
		t.Error("backoff wrong")
	}
}

// Every function must carry the discovery tags vgi-lint checks.
func TestFunctionMetadataIsComplete(t *testing.T) {
	metas := map[string]vgi.FunctionMetadata{
		"greet":          (&Greet{}).Metadata(),
		"greet/style":    (&GreetWithStyle{}).Metadata(),
		"sequence_table": (&SequenceTable{}).Metadata(),
		"fetch_items":    (&FetchItems{}).Metadata(),
	}
	for name, m := range metas {
		for _, key := range []string{"vgi.title", "vgi.category", "vgi.keywords", "vgi.doc_llm", "vgi.doc_md", "vgi.example_queries"} {
			if m.Tags[key] == "" {
				t.Errorf("%s: missing %s", name, key)
			}
		}
		var examples []exampleQuery
		if err := json.Unmarshal([]byte(m.Tags["vgi.example_queries"]), &examples); err != nil || len(examples) == 0 {
			t.Errorf("%s: bad vgi.example_queries: %v", name, err)
		}
		if len(m.Examples) == 0 || m.Description == "" {
			t.Errorf("%s: needs a description and examples", name)
		}
	}
	for _, key := range []string{"vgi.title", "vgi.doc_llm", "vgi.doc_md", "vgi.source_url", "vgi.license", "vgi.agent_test_tasks"} {
		if CatalogTags[key] == "" {
			t.Errorf("catalog: missing %s", key)
		}
	}
	if len(CatalogDocMD) < 300 {
		t.Error("catalog vgi.doc_md must be at least 300 characters")
	}
}

func TestNewWorkerRegisters(t *testing.T) {
	if NewWorker() == nil {
		t.Fatal("NewWorker returned nil")
	}
}
