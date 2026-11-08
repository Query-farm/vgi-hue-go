// Table function example: fetch_items, wrapping a paginated REST API.
//
// This is the pattern most workers start from: page through an HTTP API and
// stream each page to DuckDB as one Arrow batch.
//
//   - Streaming. Process fetches one page per call and keeps the next page
//     number in FetchState, so a 10,000-page API never sits in memory. Over
//     HTTP each call can arrive on a new request, so the cursor lives in an
//     exported field that the SDK serializes into the continuation token.
//   - Politeness. Requests time out, and 429/5xx responses are retried with
//     backoff that honours Retry-After.
//   - Credentials. A bearer token comes from a DuckDB secret if one exists
//     (CREATE SECRET (TYPE starter_api, TOKEN '...')), otherwise from the
//     STARTER_API_TOKEN environment variable. The token is read on every
//     call and never stored in state, because state travels back to DuckDB
//     over HTTP.
//
// The tests run against test/mock_api/server.py; point base_url at your real
// API once you adapt the parsing in parsePage.

package starter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

const (
	// SecretType is the DuckDB secret type fetch_items reads its token from.
	SecretType = "starter_api"
	// TokenEnvVar is the fallback when no secret exists.
	TokenEnvVar = "STARTER_API_TOKEN"
	// MaxAttempts bounds retries of 429/5xx responses.
	MaxAttempts = 4
)

// userAgent identifies the worker to the API.
const userAgent = "vgi-starter-worker/" + Version

// Timeout bounds every request.
const Timeout = 30 * time.Second

// APISecretType is registered with DuckDB so users can run
// CREATE SECRET (TYPE starter_api, TOKEN '...').
var APISecretType = vgi.SecretTypeSpec{
	Name:        SecretType,
	Description: "Bearer token for the API that fetch_items reads.",
	Schema: arrow.NewSchema([]arrow.Field{
		{Name: "token", Type: arrow.BinaryTypes.String, Nullable: true,
			Metadata: arrow.NewMetadata([]string{"redact"}, []string{"true"})},
	}, nil),
}

var fetchSchema = arrow.NewSchema([]arrow.Field{
	column("id", arrow.PrimitiveTypes.Int64, "Item identifier assigned by the API.", false),
	column("name", arrow.BinaryTypes.String, "Item display name.", true),
	column("category", arrow.BinaryTypes.String, "Item category, e.g. 'hardware'.", true),
	column("price", arrow.PrimitiveTypes.Float64, "Item price in US dollars.", true),
}, nil)

// fetchArgs declares fetch_items(base_url, page_size := 100).
type fetchArgs struct {
	BaseURL  string `vgi:"pos=0,pattern=^https?://,doc=Root URL of the API, e.g. 'https://vgi-starter-api.query-farm.services/v1'."`
	PageSize int64  `vgi:"default=100,ge=1,le=1000,doc=Items requested per page; 1 to 1000."`
}

// FetchState is the cursor: the next page to request, or 0 once the API says
// there are no more. Arguments are copied in so Process never re-parses them.
type FetchState struct {
	NextPage int64
	BaseURL  string
	PageSize int64
}

// Item is one row as the API returns it. Pointers keep JSON nulls as NULLs.
type Item struct {
	ID       int64    `json:"id"`
	Name     *string  `json:"name"`
	Category *string  `json:"category"`
	Price    *float64 `json:"price"`
}

// page is one API response.
type page struct {
	Items    []Item `json:"items"`
	NextPage *int64 `json:"next_page"`
}

// parsePage splits one API response into rows and the next page number (0 =
// done). Adapt this to your API.
func parsePage(body []byte) ([]Item, int64, error) {
	var p page
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, 0, fmt.Errorf("decoding API response: %w", err)
	}
	if p.NextPage == nil {
		return p.Items, 0, nil
	}
	return p.Items, *p.NextPage, nil
}

// token returns the bearer token: DuckDB secret first, then the environment.
func token(secrets vgi.Secrets) string {
	for _, s := range secrets.OfType(SecretType) {
		if v := vgi.RenderSecretValue(s["token"]); v != "" {
			return v
		}
	}
	return os.Getenv(TokenEnvVar)
}

// pageURL builds <base_url>/items?page=N&page_size=M.
func pageURL(baseURL string, pageNum, pageSize int64) string {
	q := url.Values{}
	q.Set("page", strconv.FormatInt(pageNum, 10))
	q.Set("page_size", strconv.FormatInt(pageSize, 10))
	return strings.TrimRight(baseURL, "/") + "/items?" + q.Encode()
}

// client is shared so connections are reused across pages.
var client = &http.Client{Timeout: Timeout}

// sleep is replaced in tests so retries don't wait.
var sleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// retryDelay honours an integer Retry-After header, else backs off exponentially.
func retryDelay(header string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, 30*time.Second)
	}
	return time.Duration(500<<attempt) * time.Millisecond
}

// getJSON GETs rawURL and returns the body, retrying 429 and 5xx with backoff.
func getJSON(ctx context.Context, rawURL, bearer string) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			if attempt >= MaxAttempts || errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("GET %s failed: %w", rawURL, err)
			}
			sleep(ctx, retryDelay("", attempt))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return body, readErr
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt >= MaxAttempts {
			snippet := string(body)
			if len(snippet) > 500 {
				snippet = snippet[:500]
			}
			return nil, fmt.Errorf("GET %s failed with HTTP %d %s: %s", rawURL, resp.StatusCode, http.StatusText(resp.StatusCode), snippet)
		}
		sleep(ctx, retryDelay(resp.Header.Get("Retry-After"), attempt))
	}
}

// buildItems turns parsed rows into Arrow columns in fetchSchema order.
func buildItems(items []Item) []arrow.Array {
	mem := memory.NewGoAllocator()
	ids := array.NewInt64Builder(mem)
	names := array.NewStringBuilder(mem)
	cats := array.NewStringBuilder(mem)
	prices := array.NewFloat64Builder(mem)
	for _, b := range []array.Builder{ids, names, cats, prices} {
		defer b.Release()
	}
	for _, it := range items {
		ids.Append(it.ID)
		appendString(names, it.Name)
		appendString(cats, it.Category)
		if it.Price == nil {
			prices.AppendNull()
		} else {
			prices.Append(*it.Price)
		}
	}
	return []arrow.Array{ids.NewArray(), names.NewArray(), cats.NewArray(), prices.NewArray()}
}

func appendString(b *array.StringBuilder, v *string) {
	if v == nil {
		b.AppendNull()
	} else {
		b.Append(*v)
	}
}

var fetchExamples = []vgi.CatalogExample{
	{SQL: "SELECT id, name, price FROM starter.main.fetch_items('https://vgi-starter-api.query-farm.services/v1') " +
		"WHERE category = 'hardware' ORDER BY price DESC LIMIT 5",
		Description: "The five priciest hardware items from the public demo API"},
	{SQL: "SELECT category, count(*) AS items, avg(price) AS avg_price " +
		"FROM starter.main.fetch_items('https://vgi-starter-api.query-farm.services/v1', page_size := 10) " +
		"GROUP BY category ORDER BY category",
		Description: "Summarize the API's items by category, ten per request"},
}

// FetchItems pages through <base_url>/items and streams every item as a row.
type FetchItems struct{}

var _ vgi.TypedTableFunc[FetchState] = (*FetchItems)(nil)

// Name is the SQL name.
func (*FetchItems) Name() string { return "fetch_items" }

// Metadata describes the function to DuckDB, agents and vgi-lint.
func (*FetchItems) Metadata() vgi.FunctionMetadata {
	tags := objectTags(docs{
		Title:    "Fetch Items From API",
		Category: "api",
		Keywords: "rest api, http, pagination, fetch, items, json api, bearer token",
		DocLLM: "Read all items from a paginated JSON REST API at base_url and return them as " +
			"rows (id, name, category, price). Pages are requested page_size at a time " +
			"(named argument, default 100) and streamed to DuckDB as they arrive. If a " +
			"DuckDB secret of type starter_api exists its token is sent as a bearer " +
			"token; otherwise the STARTER_API_TOKEN environment variable is used. " +
			"Rate-limit (429) and server errors are retried with backoff.",
		DocMD: "## fetch_items(base_url, page_size := 100)\n\n" +
			"Every item from a paginated REST API, streamed one page per batch.\n\n" +
			"Authenticate with a starter_api secret or the STARTER_API_TOKEN " +
			"environment variable:\n\n" +
			"```sql\nCREATE SECRET (TYPE starter_api, TOKEN 'your-token');\n```",
	})
	tags["vgi.result_columns_schema"] = mustJSON([]resultColumn{
		{Name: "id", Type: "BIGINT", Description: "Item identifier assigned by the API."},
		{Name: "name", Type: "VARCHAR", Description: "Item display name."},
		{Name: "category", Type: "VARCHAR", Description: "Item category, e.g. 'hardware'."},
		{Name: "price", Type: "DOUBLE", Description: "Item price in US dollars."},
	})
	return vgi.FunctionMetadata{
		Description:     "Read every item from a paginated REST API, one page per batch",
		Stability:       vgi.StabilityVolatile,
		Tags:            withExamples(tags, fetchExamples),
		RequiredSecrets: []vgi.SecretRequirement{{SecretType: SecretType}},
		Examples:        fetchExamples,
	}
}

// ArgumentSpecs derives the signature from fetchArgs' tags.
func (*FetchItems) ArgumentSpecs() []vgi.ArgSpec { return vgi.DeriveArgSpecs(fetchArgs{}) }

// OnBind declares the fixed output schema.
func (*FetchItems) OnBind(_ *vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindSchema(fetchSchema)
}

// NewState starts at page 1.
func (*FetchItems) NewState(params *vgi.ProcessParams) (*FetchState, error) {
	var args fetchArgs
	if err := vgi.BindArgs(params.Args, &args); err != nil {
		return nil, err
	}
	return &FetchState{NextPage: 1, BaseURL: args.BaseURL, PageSize: args.PageSize}, nil
}

// Process fetches one page and emits it; it finishes when the API has no next page.
func (*FetchItems) Process(ctx context.Context, params *vgi.ProcessParams, state *FetchState, out *vgirpc.OutputCollector) error {
	if state.NextPage == 0 {
		return out.Finish()
	}
	body, err := getJSON(ctx, pageURL(state.BaseURL, state.NextPage, state.PageSize), token(params.Secrets))
	if err != nil {
		return err
	}
	items, next, err := parsePage(body)
	if err != nil {
		return err
	}
	cols := buildItems(items)
	for _, c := range cols {
		defer c.Release()
	}
	state.NextPage = next
	return out.EmitArrays(cols, int64(len(items)))
}

// NewFetchItems returns the registration-ready function.
func NewFetchItems() vgi.TableFunction { return vgi.AsTableFunction[FetchState](&FetchItems{}) }
