// Package serve is the command-line front end shared by the worker and the
// recipes binary: it parses the transport and logging flags and runs a
// *vgi.Worker on the transport they select. The SDK does not parse argv.
//
//	(no flags)                    stdio: what DuckDB launches by default
//	--http [--host H] [--port N]  HTTP; prints PORT:<n> on stdout once listening
//	--unix PATH                   AF_UNIX socket (DuckDB's launch: transport);
//	                              prints UNIX:<path> on stdout once listening
//
// Logging goes to stderr: --debug, --log-level, --log-format, --log-logger.
package serve

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
)

// Options are the parsed command-line flags.
type Options struct {
	HTTP        bool
	Host        string
	Port        int
	Unix        string
	IdleTimeout time.Duration
}

// valueFlags are the flags that take a value as the next token.
var valueFlags = map[string]bool{
	"host": true, "port": true, "unix": true, "idle-timeout": true,
	"log-level": true, "log-format": true, "log-logger": true,
}

// Parse parses args (normally os.Args[1:]) and applies the logging flags.
//
// DuckDB's launcher may add flags this binary doesn't know (it varies argv to
// build distinct pool keys), so unknown flags are ignored rather than fatal.
func Parse(name string, args []string, stderr io.Writer) (Options, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o Options
	var idle float64
	fs.BoolVar(&o.HTTP, "http", false, "serve over HTTP instead of stdio")
	fs.StringVar(&o.Host, "host", "127.0.0.1", "HTTP bind host")
	fs.IntVar(&o.Port, "port", 0, "HTTP bind port (0 picks a free port)")
	fs.StringVar(&o.Unix, "unix", "", "serve on this AF_UNIX socket path (DuckDB's launch: transport)")
	fs.Float64Var(&idle, "idle-timeout", 300, "with --unix, exit after N idle seconds (0 = never)")
	logFlags := vgi.RegisterLoggingFlags(fs)
	if err := fs.Parse(knownFlags(fs, args)); err != nil {
		return o, err
	}
	if err := logFlags.Apply(); err != nil {
		return o, fmt.Errorf("logging flags: %w", err)
	}
	if o.HTTP && o.Unix != "" {
		return o, fmt.Errorf("--http and --unix are mutually exclusive")
	}
	if o.Port < 0 || o.Port > 65535 {
		return o, fmt.Errorf("--port must be in 0..65535")
	}
	o.IdleTimeout = time.Duration(idle * float64(time.Second))
	return o, nil
}

// knownFlags drops tokens for flags fs doesn't define, and stray positionals.
func knownFlags(fs *flag.FlagSet, args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" || a == "-help" {
			out = append(out, a)
			continue
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, inline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if fs.Lookup(name) == nil {
			continue
		}
		out = append(out, a)
		if valueFlags[name] && !inline && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

// Run serves w on the transport o selects, blocking until it stops.
func Run(w *vgi.Worker, o Options) error {
	switch {
	case o.Unix != "":
		return w.RunUnix(o.Unix, o.IdleTimeout)
	case o.HTTP:
		return w.RunHttp(net.JoinHostPort(o.Host, strconv.Itoa(o.Port)))
	default:
		w.RunStdio()
		return nil
	}
}

// Main parses args (normally os.Args[1:]), builds the worker and serves it.
// Errors go to stderr with exit status 2 (bad flags) or 1 (serve failure).
func Main(name string, args []string, build func() *vgi.Worker) {
	o, err := Parse(name, args, os.Stderr)
	if err == flag.ErrHelp {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(2)
	}
	if err := Run(build(), o); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}
