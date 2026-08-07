package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	mobileapp "github.com/osauer/canary/v2/internal/app"
	"github.com/osauer/canary/v2/internal/app/auth"
	apphttp "github.com/osauer/canary/v2/internal/app/http"
	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/productidentity"
)

func runApp(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			printAppUsage(os.Stdout)
			return 0
		case "pair":
			return runAppPair(args[1:])
		case "devices":
			return runAppDevices(args[1:])
		case "status":
			return runAppStatus(args[1:])
		case "restart":
			return runAppRestart(args[1:])
		case "serve":
			return runAppServe(args[1:])
		}
	}
	return runAppServe(args)
}

func runAppStatus(args []string) int {
	opts := mobileapp.DefaultOptions(effectiveVersion())
	fs := flag.NewFlagSet(productidentity.Executable+" app status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "%s app status - inspect the local app host and alert pipeline.\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Usage: %s app status [--addr HOST:PORT] [--json]\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Flags:")
		printFlagDefaults(w, fs)
	}
	fs.Usage = func() { usage(os.Stdout) }
	addr := fs.String("addr", opts.Addr, "local app host listen address")
	asJSON := fs.Bool("json", false, "print the typed status as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return rejectUnexpectedArgument(os.Stderr, productidentity.Executable+" app status", fs, usage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := fetchAppStatus(ctx, strings.TrimSpace(*addr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app status: %v\n", productidentity.Executable, err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(status)
	} else {
		renderAppStatus(os.Stdout, status)
	}
	if !apphttp.AppStatusReady(status) {
		return 1
	}
	return 0
}

func fetchAppStatus(ctx context.Context, addr string) (apphttp.AppStatusDTO, error) {
	baseURL := "http://" + mobileapp.LoopbackAddrForLocalConnect(addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+apphttp.AppStatusPath, nil)
	if err != nil {
		return apphttp.AppStatusDTO{}, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return apphttp.AppStatusDTO{}, fmt.Errorf("connect to local app host at %s: %w (start it with `%s app`)", baseURL, err, productidentity.Executable)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return apphttp.AppStatusDTO{}, err
	}
	if res.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if body.Error == "" {
			body.Error = res.Status
		}
		return apphttp.AppStatusDTO{}, errors.New(body.Error)
	}
	var status apphttp.AppStatusDTO
	if err := json.Unmarshal(raw, &status); err != nil {
		return apphttp.AppStatusDTO{}, fmt.Errorf("decode app status: %w", err)
	}
	if status.SchemaVersion != apphttp.AppStatusSchemaVersion {
		return apphttp.AppStatusDTO{}, fmt.Errorf("unsupported app status schema %q", status.SchemaVersion)
	}
	return status, nil
}

func renderAppStatus(w io.Writer, status apphttp.AppStatusDTO) {
	fmt.Fprintf(w, "Canary app %s (%s)\n", strings.ToUpper(status.State), status.Version)
	producer := "not initialized"
	if status.AlertProducer.Initialized && status.AlertProducer.Coverage != nil {
		coverage := status.AlertProducer.Coverage
		producer = fmt.Sprintf("%d/%d sources covered, %s", len(coverage.CoveredSources), len(coverage.ExpectedSources), coverage.Freshness)
	}
	fmt.Fprintf(w, "  Alert producer    %s\n", producer)
	dispatcher := status.AlertDispatcher.State
	if status.AlertDispatcher.Class != "" {
		dispatcher += " (" + status.AlertDispatcher.Class + ")"
	}
	fmt.Fprintf(w, "  Alert dispatcher  %s\n", nonEmptyAppStatus(dispatcher, "unknown"))
}

func nonEmptyAppStatus(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func runAppRestart(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return cli.RunRestart(ctx, append([]string{"--app"}, args...), os.Stdout, os.Stderr)
}

func runAppServe(args []string) int {
	opts := mobileapp.DefaultOptions(effectiveVersion())
	fs := flag.NewFlagSet(productidentity.Executable+" app", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		printAppUsage(os.Stdout)
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Serve flags:")
		printFlagDefaults(os.Stdout, fs)
	}
	addr := fs.String("addr", opts.Addr, "HTTP listen address")
	publicURL := fs.String("public-url", opts.PublicURL, "trusted browser-visible base URL")
	remote := fs.Bool("remote", opts.Remote, "enable the outbound Cloudflare Worker relay")
	remoteURL := fs.String("remote-url", opts.RemoteURL, "Cloudflare Worker relay base URL")
	stateDir := fs.String("state-dir", opts.StateDir, "local app state directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return rejectUnexpectedArgument(os.Stderr, productidentity.Executable+" app", fs, func(w io.Writer) {
			printAppUsage(w)
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Serve flags:")
			printFlagDefaults(w, fs)
		})
	}
	opts.Addr = strings.TrimSpace(*addr)
	opts.Remote = *remote
	opts.RemoteURL = strings.TrimRight(strings.TrimSpace(*remoteURL), "/")
	if flagWasSet(fs, "public-url") {
		opts.PublicURL = strings.TrimRight(strings.TrimSpace(*publicURL), "/")
	} else if !opts.PublicURLFromEnv {
		opts.PublicURL = mobileapp.PublicURLForAddr(opts.Addr)
	}
	opts.StateDir = strings.TrimSpace(*stateDir)

	app, err := mobileapp.New(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app: %v\n", productidentity.Executable, err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stdout, "%s app serving %s (listen %s)\n", productidentity.Executable, app.Options.PublicURL, app.Options.Addr)
	fmt.Fprintf(os.Stdout, "Pair a phone with: %s app pair\n", productidentity.Executable)
	if err := app.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%s app: %v\n", productidentity.Executable, err)
		return 1
	}
	return 0
}

func runAppPair(args []string) int {
	opts := mobileapp.DefaultOptions(effectiveVersion())
	fs := flag.NewFlagSet(productidentity.Executable+" app pair", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairUsage := func(w io.Writer) {
		fmt.Fprintf(w, "%s app pair - print a short-lived QR pairing URL from the local app host.\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Usage: %s app pair [--addr HOST:PORT] [--public-url URL] [--json]\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Flags:")
		printFlagDefaults(w, fs)
	}
	fs.Usage = func() { pairUsage(os.Stdout) }
	addr := fs.String("addr", opts.Addr, "local app host listen address")
	publicURLDefault := ""
	if opts.PublicURLFromEnv {
		publicURLDefault = opts.PublicURL
	}
	publicURL := fs.String("public-url", publicURLDefault, "override browser-visible base URL to embed in the pairing QR")
	asJSON := fs.Bool("json", false, "print the pairing session as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return rejectUnexpectedArgument(os.Stderr, productidentity.Executable+" app pair", fs, pairUsage)
	}
	pairAddr := strings.TrimSpace(*addr)
	pairPublicURL := appPairPublicURLOverride(fs, *publicURL, opts.PublicURLFromEnv)
	session, err := createPairingSession(pairAddr, pairPublicURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app pair: %v\n", productidentity.Executable, err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(session)
		return 0
	}
	qr, err := qrcode.New(session.URL, qrcode.Medium)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app pair: QR: %v\n", productidentity.Executable, err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "Scan this QR code with the iPhone:")
	fmt.Fprintln(os.Stdout)
	fmt.Fprint(os.Stdout, qr.ToSmallString(false))
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "Pairing URL: %s\n", session.URL)
	fmt.Fprintf(os.Stdout, "Expires: %s\n", session.ExpiresAt.Local().Format(time.RFC1123))
	return 0
}

func runAppDevices(args []string) int {
	prune := false
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "prune":
			prune = true
			args = args[1:]
		case "list":
			args = args[1:]
		default:
			fmt.Fprintf(os.Stderr, "%s app devices: unknown subcommand %q (try `%s app devices` or `%s app devices prune`)\n", productidentity.Executable, args[0], productidentity.Executable, productidentity.Executable)
			return 2
		}
	}
	opts := mobileapp.DefaultOptions(effectiveVersion())
	fs := flag.NewFlagSet(productidentity.Executable+" app devices", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	devicesUsage := func(w io.Writer) {
		fmt.Fprintf(w, "%s app devices - list or prune paired device grants on the local app host.\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintf(w, "  %s app devices [--addr HOST:PORT] [--json]\n", productidentity.Executable)
		fmt.Fprintf(w, "  %s app devices prune [--keep-days N] [--addr HOST:PORT] [--json]\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Flags:")
		printFlagDefaults(w, fs)
	}
	fs.Usage = func() { devicesUsage(os.Stdout) }
	addr := fs.String("addr", opts.Addr, "local app host listen address")
	keepDays := fs.Int("keep-days", 7, "prune grants whose last activity is older than this many days")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return rejectUnexpectedArgument(os.Stderr, productidentity.Executable+" app devices", fs, devicesUsage)
	}
	baseURL := "http://" + mobileapp.LoopbackAddrForLocalConnect(strings.TrimSpace(*addr))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var req *http.Request
	var err error
	if prune {
		body, merr := json.Marshal(struct {
			KeepDays int `json:"keep_days"`
		}{KeepDays: *keepDays})
		if merr != nil {
			fmt.Fprintf(os.Stderr, "%s app devices: %v\n", productidentity.Executable, merr)
			return 1
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/devices/prune", bytes.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/devices", nil)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app devices: %v\n", productidentity.Executable, err)
		return 1
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app devices: connect to local app host at %s: %v (start it with `%s app`)\n", productidentity.Executable, baseURL, err, productidentity.Executable)
		return 1
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s app devices: %v\n", productidentity.Executable, err)
		return 1
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = res.Status
		}
		fmt.Fprintf(os.Stderr, "%s app devices: %s\n", productidentity.Executable, e.Error)
		return 1
	}
	if *asJSON {
		fmt.Fprintln(os.Stdout, strings.TrimSpace(string(raw)))
		return 0
	}
	if prune {
		var out struct {
			Removed  int `json:"removed"`
			Kept     int `json:"kept"`
			KeepDays int `json:"keep_days"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			fmt.Fprintf(os.Stderr, "%s app devices: decode response: %v\n", productidentity.Executable, err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "Pruned %d device grant(s) not seen in the last %d day(s); %d kept.\n", out.Removed, out.KeepDays, out.Kept)
		return 0
	}
	var out struct {
		Devices []struct {
			ID         string    `json:"id"`
			Name       string    `json:"name"`
			CreatedAt  time.Time `json:"created_at"`
			LastSeenAt time.Time `json:"last_seen_at"`
		} `json:"devices"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fmt.Fprintf(os.Stderr, "%s app devices: decode response: %v\n", productidentity.Executable, err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "%d paired device grant(s):\n", out.Total)
	for _, d := range out.Devices {
		seen := "never"
		if !d.LastSeenAt.IsZero() {
			seen = d.LastSeenAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(os.Stdout, "  %-24s %-10s paired %s  last seen %s\n", d.ID, d.Name, d.CreatedAt.Local().Format("2006-01-02 15:04"), seen)
	}
	return 0
}

func createPairingSession(addr, publicURL string) (auth.PairingSession, error) {
	baseURL := "http://" + mobileapp.LoopbackAddrForLocalConnect(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := []byte("{}")
	if strings.TrimSpace(publicURL) != "" {
		var err error
		body, err = json.Marshal(struct {
			PublicURL string `json:"public_url,omitempty"`
		}{PublicURL: strings.TrimRight(strings.TrimSpace(publicURL), "/")})
		if err != nil {
			return auth.PairingSession{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/pairing/sessions", bytes.NewReader(body))
	if err != nil {
		return auth.PairingSession{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return auth.PairingSession{}, fmt.Errorf("connect to local app host at %s: %w (start it with `%s app`)", baseURL, err, productidentity.Executable)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		if body.Error == "" {
			body.Error = res.Status
		}
		return auth.PairingSession{}, errors.New(body.Error)
	}
	var session auth.PairingSession
	if err := json.NewDecoder(res.Body).Decode(&session); err != nil {
		return auth.PairingSession{}, err
	}
	return session, nil
}

func appPairPublicURLOverride(fs *flag.FlagSet, publicURL string, defaultIsExplicit bool) string {
	if !flagWasSet(fs, "public-url") && !defaultIsExplicit {
		return ""
	}
	return strings.TrimSpace(publicURL)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

func printAppUsage(w io.Writer) {
	fmt.Fprintf(w, "%s app - run the paired mobile PWA application layer.\n", productidentity.Executable)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  %s app [--addr HOST:PORT] [--public-url URL] [--remote] [--remote-url URL] [--state-dir PATH]\n", productidentity.Executable)
	fmt.Fprintf(w, "  %s app restart [--addr HOST:PORT] [--public-url URL] [--remote] [--remote-url URL] [--state-dir PATH]\n", productidentity.Executable)
	fmt.Fprintf(w, "  %s app pair [--addr HOST:PORT] [--public-url URL] [--json]\n", productidentity.Executable)
	fmt.Fprintf(w, "  %s app devices [prune] [--keep-days N] [--addr HOST:PORT] [--json]\n", productidentity.Executable)
	fmt.Fprintf(w, "  %s app status [--addr HOST:PORT] [--json]\n", productidentity.Executable)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The app serves a mobile-first PWA, live SSE snapshots,")
	fmt.Fprintln(w, "and opt-in Canary Web Push subscriptions. Pairing URLs are short-lived.")
}

func printFlagDefaults(w io.Writer, fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(w, "  --%-12s  %s (default %q)\n", f.Name, f.Usage, f.DefValue)
	})
}

// rejectUnexpectedArgument reports a stray positional argument and prints
// the command usage, suggesting the "--" spelling when the argument names a
// defined flag — `canary app remote` should teach `--remote`, not dead-end.
func rejectUnexpectedArgument(w io.Writer, prefix string, fs *flag.FlagSet, usage func(io.Writer)) int {
	arg := fs.Arg(0)
	fmt.Fprintf(w, "%s: unexpected argument %q", prefix, arg)
	if name := strings.TrimLeft(arg, "-"); name != "" && fs.Lookup(name) != nil {
		fmt.Fprintf(w, " (did you mean --%s?)", name)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	usage(w)
	return 2
}
