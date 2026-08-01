// Command mainspring is a backend-agnostic local inference server: one stable
// OpenAI-compatible API in front, pluggable engines (llama.cpp today) behind it.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/backend/llamacpp"
	"github.com/ankit373/mainspring/internal/backend/lmstudio"
	"github.com/ankit373/mainspring/internal/backend/mlx"
	"github.com/ankit373/mainspring/internal/backend/ollama"
	"github.com/ankit373/mainspring/internal/backend/openaiadopt"
	"github.com/ankit373/mainspring/internal/breaker"
	"github.com/ankit373/mainspring/internal/build"
	"github.com/ankit373/mainspring/internal/config"
	"github.com/ankit373/mainspring/internal/install"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

func main() {
	root := &cobra.Command{
		Use:           "mainspring",
		Short:         "Backend-agnostic local inference server",
		Long:          "Mainspring serves local models over a stable OpenAI-compatible API,\nrunning them on pluggable engine backends (llama.cpp today).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("config", "", "config file (default: ~/.config/mainspring/config.yaml)")
	root.AddCommand(cmdServe(), cmdBackends(), cmdModels(), cmdInstall(), cmdDoctor(), cmdVersion())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(build.String())
		},
	}
}

func cmdBackends() *cobra.Command {
	return &cobra.Command{
		Use:   "backends",
		Short: "Show inference engine backends detected on this host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			fmt.Printf("%-12s %-9s %-9s %s\n", "BACKEND", "PRESENT", "SOURCE", "DETAIL")
			for _, b := range allBackends(cfg) {
				av := b.Detect(ctx)
				if _, ok := install.ManagedPath(av.Name); ok {
					av.Managed = true
				}
				source := "-"
				if av.Present {
					source = "system"
					if av.Managed {
						source = "managed"
					}
				}
				detail := av.Path
				if av.Version != "" {
					detail += "  " + av.Version
				}
				if !av.Present {
					detail = av.Reason
				}
				fmt.Printf("%-12s %-9v %-9s %s\n", av.Name, av.Present, source, detail)
			}
			fmt.Println("\nadopt-only backends detect a running local OpenAI server; Mainspring never installs them.")
			return nil
		},
	}
}

func cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the Mainspring environment (backends, config, models, ports)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Println(build.String())
			cfg, err := config.Load(configPath(cmd))
			if err != nil {
				fmt.Printf("✗ config: %v\n", err)
				return nil
			}
			fmt.Println("✓ config loaded")

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			present := map[string]bool{}
			fmt.Println("\nbackends:")
			for _, b := range allBackends(cfg) {
				av := b.Detect(ctx)
				present[av.Name] = av.Present
				mark := "✗"
				detail := av.Reason
				if av.Present {
					mark = "✓"
					detail = av.Version
					if _, ok := install.ManagedPath(av.Name); ok {
						detail += " (managed)"
					}
				}
				fmt.Printf("  %s %-10s %s\n", mark, av.Name, detail)
			}

			fmt.Println("\nmodels:")
			if len(cfg.Models) == 0 {
				fmt.Println("  (none configured)")
			}
			for _, line := range checkModels(cfg, present) {
				fmt.Println("  " + line)
			}

			fmt.Println("\nconfig lint:")
			if warnings := cfg.Lint(); len(warnings) == 0 {
				fmt.Println("  ✓ no issues found")
			} else {
				for _, w := range warnings {
					fmt.Println("  ⚠ " + w.String())
				}
			}

			fmt.Println("\nserver:")
			if portFree(cfg.Addr) {
				fmt.Printf("  ✓ listen address %s is free\n", cfg.Addr)
			} else {
				fmt.Printf("  ✗ listen address %s is in use\n", cfg.Addr)
			}
			fmt.Printf("  · accelerator: %s\n", gpuHint())
			return nil
		},
	}
}

// checkModels resolves each configured model's backend and reports whether that
// backend is present and (for local-file backends) whether the weights exist.
func checkModels(cfg config.Config, present map[string]bool) []string {
	fallback := defaultBackendName(cfg)
	var lines []string
	for _, m := range cfg.Models {
		b := m.Backend
		if b == "" {
			b = fallback
		}
		switch {
		case !present[b]:
			lines = append(lines, fmt.Sprintf("✗ %s → %s (backend not available)", m.ID, b))
		case (b == "llamacpp" || b == "mlx") && m.Path != "" && !pathExists(m.Path):
			lines = append(lines, fmt.Sprintf("✗ %s → %s (weights not found: %s)", m.ID, b, m.Path))
		default:
			lines = append(lines, fmt.Sprintf("✓ %s → %s", m.ID, b))
		}
	}
	return lines
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// portFree reports whether addr can be bound (i.e. is currently free).
func portFree(addr string) bool {
	if addr == "" {
		addr = ":11500"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// gpuHint gives a best-effort accelerator note for this platform.
func gpuHint() string {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "Apple Silicon (Metal) — use backend llamacpp or mlx"
	case runtime.GOOS == "linux":
		return "Linux — check `nvidia-smi` (CUDA) or ROCm; ensure the engine build has GPU support"
	default:
		return runtime.GOOS + "/" + runtime.GOARCH
	}
}

func cmdInstall() *cobra.Command {
	var version, url, sha, manifest, sig, pubkey string
	cmd := &cobra.Command{
		Use:   "install <backend>",
		Short: "Install an engine backend binary (opt-in, checksum-verified)",
		Long:  "Downloads and SHA-256-verifies an engine binary, then records a receipt so\nthe backend prefers the managed binary. Nothing is downloaded unless you run\nthis command. Pin a specific artifact with --url and --sha256, or rely on a\nmanifest (--manifest / ~/.config/mainspring/install.json). When --pubkey (or\n$MAINSPRING_MANIFEST_PUBKEY) is set, the manifest must carry a valid ed25519\ndetached signature.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if pubkey == "" {
				pubkey = os.Getenv("MAINSPRING_MANIFEST_PUBKEY")
			}
			rec, err := install.Install(cmd.Context(), args[0], install.Spec{
				Version: version, URL: url, SHA256: sha, PubKey: pubkey, SigPath: sig,
			}, manifest, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			_ = rec
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "version to install (from manifest)")
	cmd.Flags().StringVar(&url, "url", "", "pin a direct download URL (requires --sha256)")
	cmd.Flags().StringVar(&sha, "sha256", "", "expected SHA-256 of the download")
	cmd.Flags().StringVar(&manifest, "manifest", "", "manifest file (default: ~/.config/mainspring/install.json)")
	cmd.Flags().StringVar(&pubkey, "pubkey", "", "base64 ed25519 public key; requires a signed manifest ($MAINSPRING_MANIFEST_PUBKEY)")
	cmd.Flags().StringVar(&sig, "manifest-sig", "", "detached manifest signature file (default: <manifest>.sig)")
	return cmd
}

func cmdModels() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List configured models",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath(cmd))
			if err != nil {
				return err
			}
			if len(cfg.Models) == 0 {
				fmt.Println("no models configured — add them to your config or pass --model id=path to serve")
				return nil
			}
			fallback := defaultBackendName(cfg)
			fmt.Printf("%-24s %-10s %-6s %s\n", "MODEL", "BACKEND", "CTX", "PATH")
			for _, m := range cfg.Models {
				b := m.Backend
				if b == "" {
					b = fallback
				}
				fmt.Printf("%-24s %-10s %-6d %s\n", m.ID, b, m.Ctx, m.Path)
			}
			return nil
		},
	}
}

func cmdServe() *cobra.Command {
	var (
		addr        string
		modelFlags  []string
		apiKeys     []string
		keepAlive   int
		maxLoaded   int
		maxResident int
		maxInflight int
		maxQueue    int
		usageLedger string
		backendName string
		ollamaHost  string
		llamaServer string
		tlsCert     string
		tlsKey      string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the OpenAI-compatible inference server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath(cmd))
			if err != nil {
				return err
			}
			// Flag overrides.
			if cmd.Flags().Changed("addr") {
				cfg.Addr = addr
			}
			if cmd.Flags().Changed("api-key") {
				cfg.APIKeys = apiKeys
			}
			if cmd.Flags().Changed("keep-alive") {
				cfg.KeepAliveSeconds = keepAlive
			}
			if cmd.Flags().Changed("max-loaded") {
				cfg.MaxLoaded = maxLoaded
			}
			if cmd.Flags().Changed("max-resident-mb") {
				cfg.MaxResidentMB = maxResident
			}
			if cmd.Flags().Changed("max-inflight") {
				cfg.MaxInflight = maxInflight
			}
			if cmd.Flags().Changed("max-queue") {
				cfg.MaxQueue = maxQueue
			}
			if cmd.Flags().Changed("usage-ledger") {
				cfg.UsageLedger = usageLedger
			}
			if cmd.Flags().Changed("backend") {
				cfg.Backend = backendName
			}
			if cmd.Flags().Changed("ollama-host") {
				cfg.OllamaHost = ollamaHost
			}
			if cmd.Flags().Changed("llama-server") {
				cfg.LlamaServerPath = llamaServer
			}
			if cmd.Flags().Changed("tls-cert") {
				cfg.TLSCert = tlsCert
			}
			if cmd.Flags().Changed("tls-key") {
				cfg.TLSKey = tlsKey
			}
			for _, mf := range modelFlags {
				m, err := parseModelFlag(mf)
				if err != nil {
					return err
				}
				cfg.Models = append(cfg.Models, m)
			}
			if len(cfg.Models) == 0 && !cfg.DiscoverModels {
				return fmt.Errorf("no models configured — pass --model id=/path/to/weights.gguf, add models to the config, or set discover_models: true")
			}

			return runServe(cmd.Context(), cfg, configPath(cmd))
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":11500", "listen address")
	cmd.Flags().StringArrayVar(&modelFlags, "model", nil, "model as id=/path/to/weights.gguf (repeatable)")
	cmd.Flags().StringArrayVar(&apiKeys, "api-key", nil, "require this API key (repeatable); if none set, server runs OPEN")
	cmd.Flags().IntVar(&keepAlive, "keep-alive", 300, "seconds to keep an idle model loaded (0 = never unload)")
	cmd.Flags().IntVar(&maxLoaded, "max-loaded", 1, "max models resident at once by count (LRU-evicted beyond this)")
	cmd.Flags().IntVar(&maxResident, "max-resident-mb", 0, "max resident memory across models in MB (0 = no byte cap)")
	cmd.Flags().IntVar(&maxInflight, "max-inflight", 0, "max concurrent requests per model (0 = unbounded)")
	cmd.Flags().IntVar(&maxQueue, "max-queue", 0, "max queued waiters per model before returning 503")
	cmd.Flags().StringVar(&usageLedger, "usage-ledger", "", "JSONL usage ledger path (empty = default location, \"off\" = disable)")
	cmd.Flags().StringVar(&backendName, "backend", "", "engine backend: llamacpp (default) | ollama | mlx")
	cmd.Flags().StringVar(&ollamaHost, "ollama-host", "", "Ollama daemon URL when --backend ollama")
	cmd.Flags().StringVar(&llamaServer, "llama-server", "", "path to llama-server (default: look up PATH)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "PEM certificate path (with --tls-key => serve HTTPS)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "PEM private key path")
	return cmd
}

// modelSpecs builds the scheduler specs from config, applying the default
// backend to models that don't name one. Shared by startup and SIGHUP reload.
func modelSpecs(cfg config.Config) []backend.ModelSpec {
	fallback := defaultBackendName(cfg)
	specs := make([]backend.ModelSpec, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		bname := m.Backend
		if bname == "" {
			bname = fallback
		}
		specs = append(specs, backend.ModelSpec{
			ID:        m.ID,
			Backend:   bname,
			Fallbacks: m.Fallbacks,
			Path:      m.Path,
			CtxSize:   m.Ctx,
			GPULayers: m.GPULayers,
			ExtraArgs: m.Args,
		})
	}
	return specs
}

// authTenants resolves the effective tenant set from config: explicit tenants
// when present, otherwise each API key as an admin tenant (mirroring auth.New).
// Shared by startup and SIGHUP reload.
func authTenants(cfg config.Config) []auth.Tenant {
	if len(cfg.Tenants) > 0 {
		ts := make([]auth.Tenant, 0, len(cfg.Tenants))
		for _, t := range cfg.Tenants {
			ts = append(ts, auth.Tenant{
				Name:        t.Name,
				Key:         t.Key,
				Role:        auth.Role(t.Role),
				RateRPM:     t.RateRPM,
				TokenBudget: t.TokenBudget,
				WindowSec:   t.WindowSec,
			})
		}
		return ts
	}
	ts := make([]auth.Tenant, 0, len(cfg.APIKeys))
	for i, k := range cfg.APIKeys {
		if k = strings.TrimSpace(k); k != "" {
			ts = append(ts, auth.Tenant{Name: fmt.Sprintf("key-%d", i+1), Key: k, Role: auth.RoleAdmin})
		}
	}
	return ts
}

func runServe(ctx context.Context, cfg config.Config, cfgPath string) error {
	startupLint := cfg.Lint()
	for _, w := range startupLint {
		fmt.Fprintln(os.Stderr, "warning: config:", w.String())
	}

	specs := modelSpecs(cfg)
	needed := map[string]bool{}
	for _, sp := range specs {
		for _, bname := range sp.Candidates() { // primary + fallbacks
			needed[bname] = true
		}
	}
	// Model auto-discovery: also construct the adopt backends so we can enumerate
	// their models. Absent ones are skipped below (not fatal).
	if cfg.DiscoverModels {
		for _, bname := range adoptBackendNames {
			needed[bname] = true
		}
	}

	// Construct and detect only the backends the configured models actually use.
	// An absent backend is not fatal when it is only a fallback: we skip it and
	// require each model to retain at least one present candidate (checked below).
	backends := make(map[string]backend.Backend, len(needed))
	for name := range needed {
		be, err := newBackendByName(name, cfg)
		if err != nil {
			return err
		}
		if av := be.Detect(ctx); !av.Present {
			fmt.Fprintf(os.Stderr, "warning: backend %q unavailable (%s) — skipping; models will fail over if configured\n", name, av.Reason)
			continue
		}
		backends[name] = be
	}
	// Every model must have at least one usable backend among its candidates.
	for _, sp := range specs {
		hasBackend := false
		for _, bname := range sp.Candidates() {
			if backends[bname] != nil {
				hasBackend = true
				break
			}
		}
		if !hasBackend {
			return fmt.Errorf("model %q has no available backend (tried %v)", sp.ID, sp.Candidates())
		}
	}

	// Auto-discover models from present adopt backends (opt-in). Configured
	// models win on an id clash.
	if cfg.DiscoverModels {
		specs = append(specs, discoverModels(ctx, backends, specs)...)
	}
	if len(specs) == 0 {
		return fmt.Errorf("no models to serve — none configured and discovery found none on any present adopt backend")
	}

	sched := scheduler.New(backends, specs, scheduler.Options{
		KeepAlive: cfg.KeepAlive(),
		MaxLoaded: cfg.MaxLoaded,
		MaxBytes:  cfg.MaxBytes(),
		Aliases:   cfg.Aliases,
	})
	if err := sched.Validate(); err != nil {
		return fmt.Errorf("invalid model aliases: %w", err)
	}
	authn := buildAuth(cfg)

	ledgerPath := cfg.UsageLedger
	if ledgerPath == "" {
		ledgerPath = config.DefaultLedgerPath()
	}
	if ledgerPath == "off" {
		ledgerPath = ""
	}
	rec, err := metrics.New(ledgerPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err) // metrics still work in-memory
	}
	defer func() { _ = rec.Close() }()

	srv := server.New(sched, authn, rec)
	srv.SetConcurrency(cfg.MaxInflight, cfg.MaxQueue)
	if cfg.BreakerThreshold > 0 {
		srv.SetBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown())
	}
	srv.SetLintWarnings(lintStrings(startupLint))
	// Wire the admin reload endpoint to the same reload path SIGHUP uses.
	srv.SetReloadFunc(func() error { return reloadConfig(cfgPath, cfg, sched, authn, srv) })
	// Per-request timeouts: global default + per-model overrides.
	perModelTimeout := map[string]time.Duration{}
	for _, m := range cfg.Models {
		if m.TimeoutS > 0 {
			perModelTimeout[m.ID] = time.Duration(m.TimeoutS) * time.Second
		}
	}
	srv.SetTimeouts(cfg.RequestTimeout(), perModelTimeout)
	// Opt-in response cache (disabled unless cache_max_entries > 0).
	if cfg.CacheMaxEntries > 0 {
		srv.SetCache(cfg.CacheTTL(), cfg.CacheMaxEntries)
	}
	// Per-model USD pricing for cost accounting (0 = free/local).
	costRates := map[string]server.CostRate{}
	for _, m := range cfg.Models {
		if m.InputUSDPerMTok > 0 || m.OutputUSDPerMTok > 0 {
			costRates[m.ID] = server.NewCostRate(m.InputUSDPerMTok, m.OutputUSDPerMTok)
		}
	}
	if len(costRates) > 0 {
		srv.SetCostRates(costRates)
	}
	// Context guardrail: reject/warn requests that exceed a model's context
	// window. Active per model only when its ctx is known (>0) and enforcement is
	// on globally or overridden true for that model.
	ctxPolicies := map[string]server.ContextPolicy{}
	for _, m := range cfg.Models {
		if m.Ctx <= 0 {
			continue
		}
		enforce := cfg.EnforceContext
		if m.EnforceContext != nil {
			enforce = *m.EnforceContext
		}
		if cfg.EnforceContext || (m.EnforceContext != nil && *m.EnforceContext) {
			ctxPolicies[m.ID] = server.ContextPolicy{Limit: m.Ctx, Enforce: enforce}
		}
	}
	if len(ctxPolicies) > 0 {
		srv.SetContextGuard(ctxPolicies)
	}
	// Precise (exact-tokenization) guardrail: per-model, active when precise is on
	// globally or overridden true for that model and the model has a known ctx.
	precise := map[string]bool{}
	for _, m := range cfg.Models {
		if m.Ctx <= 0 {
			continue
		}
		if cfg.PreciseContext || (m.PreciseContext != nil && *m.PreciseContext) {
			precise[m.ID] = true
		}
	}
	if len(precise) > 0 {
		srv.SetPreciseContext(precise)
	}
	// max_tokens clamping: per-model window, active when clamping is on globally
	// or overridden true for that model and the model has a known ctx.
	clampLimits := map[string]int{}
	for _, m := range cfg.Models {
		if m.Ctx <= 0 {
			continue
		}
		if cfg.ClampMaxTokens || (m.ClampMaxTokens != nil && *m.ClampMaxTokens) {
			clampLimits[m.ID] = m.Ctx
		}
	}
	if len(clampLimits) > 0 {
		srv.SetClampLimits(clampLimits)
	}
	// Bounded retry of transient upstream failures (disabled unless retry_max > 0).
	if cfg.RetryMax > 0 {
		srv.SetRetry(cfg.RetryMax, cfg.RetryBackoff())
	}
	// Model-level fallback chains: serve from another model when the primary is
	// unavailable (circuit open / load failure).
	modelFallbacks := map[string][]string{}
	for _, m := range cfg.Models {
		if len(m.ModelFallbacks) > 0 {
			modelFallbacks[m.ID] = m.ModelFallbacks
		}
	}
	if len(modelFallbacks) > 0 {
		srv.SetModelFallbacks(modelFallbacks)
	}
	// Single-flight request coalescing (disabled unless coalesce: true).
	if cfg.Coalesce {
		srv.SetCoalescing(true)
	}

	if alClose, err := configureAccessLog(srv, cfg.AccessLog); err != nil {
		fmt.Fprintln(os.Stderr, "warning: access log disabled:", err)
	} else if alClose != nil {
		defer alClose()
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.TLSEnabled() {
		httpSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	fmt.Println(build.String())
	fmt.Printf("serving %d model(s) on %s (%s)\n", len(specs), cfg.Addr, scheme)
	if ledgerPath != "" {
		fmt.Printf("metrics: %s/metrics   usage ledger: %s\n", cfg.Addr, ledgerPath)
	} else {
		fmt.Printf("metrics: %s/metrics   usage ledger: disabled\n", cfg.Addr)
	}
	if authn.Open() {
		fmt.Println("⚠  WARNING: no API keys configured — the server is OPEN (no authentication).")
		fmt.Println("⚠  Do not expose this address beyond localhost. Set api_keys / --api-key to require auth.")
	}

	// Warm up preloaded models in the background so the server is available
	// immediately while cold-starts happen concurrently.
	if ids := preloadIDs(cfg); len(ids) > 0 {
		if cfg.MaxLoaded > 0 && len(ids) > cfg.MaxLoaded {
			fmt.Printf("⚠  %d models set to preload but max_loaded=%d — some will be evicted as they load\n", len(ids), cfg.MaxLoaded)
		}
		go func() {
			for _, id := range ids {
				if _, err := sched.EnsureLoaded(context.Background(), id); err != nil {
					fmt.Printf("preload %s: %v\n", id, err)
				} else {
					fmt.Printf("preloaded %s\n", id)
				}
			}
		}()
	}

	// SIGHUP hot-reloads the safe subset of config (models, aliases, tenants)
	// from the config file without dropping in-flight requests or the listener.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := reloadConfig(cfgPath, cfg, sched, authn, srv); err != nil {
				fmt.Fprintln(os.Stderr, "SIGHUP:", err)
			}
		}
	}()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background health prober: feed loaded-runner health into the circuit
	// breaker so a backend that goes bad trips (and recovers) without waiting for
	// live request failures.
	if iv := cfg.HealthProbeInterval(); iv > 0 && cfg.BreakerThreshold > 0 {
		go runHealthProber(ctx, iv, sched, srv.Breaker())
	}

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if cfg.TLSEnabled() {
			serveErr = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Graceful drain: flip readiness so load balancers stop routing, let
		// in-flight requests finish (http.Server.Shutdown), then reclaim models.
		srv.SetDraining(true)
		fmt.Println("\ndraining in-flight requests…")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout())
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			fmt.Println("drain timed out; forcing shutdown")
		}
		fmt.Println("reclaiming loaded models…")
		sched.Shutdown(context.Background())
		return nil
	}
}

// runHealthProber periodically probes each loaded runner's health and feeds the
// result into the circuit breaker (StatusReady => success, anything else =>
// failure). It exits when ctx is cancelled. A "down" runner trips the breaker;
// a recovered one closes it, without waiting for live traffic to notice.
func runHealthProber(ctx context.Context, interval time.Duration, sched *scheduler.Scheduler, brk *breaker.Group) {
	if brk == nil || !brk.Enabled() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, ri := range sched.Loaded() {
				pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				status := ri.Runner.Health(pctx)
				cancel()
				brk.OnResult(ri.ID, status == backend.StatusReady)
			}
		}
	}
}

// reloadConfig re-reads the config file and applies the safe subset: model
// specs, aliases, and tenants. Every other setting is either a listener/process
// concern that cannot be swapped on a running server, or a value some other
// Set* call already baked into the server/scheduler at startup and never
// revisits — reload does not re-run any of that wiring. Rather than silently
// keeping the old value with no explanation, every changed-but-unapplied field
// is logged so the difference is never a silent surprise. A missing config file
// is a no-op (so a flag-only launch is never wiped). Shared by SIGHUP and the
// admin reload endpoint; it returns an error the caller can surface or log.
func reloadConfig(path string, startup config.Config, sched *scheduler.Scheduler, authn *auth.Authenticator, srv *server.Server) error {
	resolved := path
	if resolved == "" {
		resolved = config.DefaultPath()
	}
	if _, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("no config file at %s; reload skipped", resolved)
	}
	newCfg, err := config.Load(resolved)
	if err != nil {
		return fmt.Errorf("reload failed, keeping current config: %w", err)
	}
	warnUnappliedChanges(startup, newCfg)

	if err := sched.Reload(modelSpecs(newCfg), newCfg.Aliases); err != nil {
		return fmt.Errorf("model/alias reload rejected, keeping current: %w", err)
	}
	authn.Reload(authTenants(newCfg))
	srv.SetLintWarnings(lintStrings(newCfg.Lint()))
	fmt.Printf("reload: %d model(s), %d alias(es), %d tenant(s)\n",
		len(newCfg.Models), len(newCfg.Aliases), len(authTenants(newCfg)))
	return nil
}

// warnUnappliedChanges logs every top-level and per-model setting that reload
// does not (and, for most of these, structurally cannot without a restart)
// re-apply, whenever it differs between the running config and the newly
// loaded one. Silence otherwise — an unchanged value is not worth a line.
func warnUnappliedChanges(startup, newCfg config.Config) {
	warnRestartRequired("addr", startup.Addr, newCfg.Addr)
	warnRestartRequired("backend", startup.Backend, newCfg.Backend)
	warnRestartRequired("keep_alive_seconds", startup.KeepAliveSeconds, newCfg.KeepAliveSeconds)
	warnRestartRequired("max_loaded", startup.MaxLoaded, newCfg.MaxLoaded)
	warnRestartRequired("max_resident_mb", startup.MaxResidentMB, newCfg.MaxResidentMB)
	warnRestartRequired("max_inflight", startup.MaxInflight, newCfg.MaxInflight)
	warnRestartRequired("max_queue", startup.MaxQueue, newCfg.MaxQueue)
	warnRestartRequired("breaker_threshold", startup.BreakerThreshold, newCfg.BreakerThreshold)
	warnRestartRequired("breaker_cooldown_seconds", startup.BreakerCooldownS, newCfg.BreakerCooldownS)
	warnRestartRequired("health_probe_seconds", startup.HealthProbeS, newCfg.HealthProbeS)
	warnRestartRequired("drain_seconds", startup.DrainSeconds, newCfg.DrainSeconds)
	warnRestartRequired("usage_ledger", startup.UsageLedger, newCfg.UsageLedger)
	warnRestartRequired("access_log", startup.AccessLog, newCfg.AccessLog)
	warnRestartRequired("tls_cert", startup.TLSCert, newCfg.TLSCert)
	warnRestartRequired("tls_key", startup.TLSKey, newCfg.TLSKey)
	warnRestartRequired("llama_server_path", startup.LlamaServerPath, newCfg.LlamaServerPath)
	warnRestartRequired("ollama_host", startup.OllamaHost, newCfg.OllamaHost)
	warnRestartRequired("mlx_python", startup.MLXPython, newCfg.MLXPython)
	warnRestartRequired("lmstudio_host", startup.LMStudioHost, newCfg.LMStudioHost)
	warnRestartRequired("llamafile_host", startup.LlamafileHost, newCfg.LlamafileHost)
	warnRestartRequired("gpt4all_host", startup.GPT4AllHost, newCfg.GPT4AllHost)
	warnRestartRequired("discover_models", startup.DiscoverModels, newCfg.DiscoverModels)
	warnRestartRequired("request_timeout_seconds", startup.RequestTimeoutS, newCfg.RequestTimeoutS)
	warnRestartRequired("cache_max_entries", startup.CacheMaxEntries, newCfg.CacheMaxEntries)
	warnRestartRequired("cache_ttl_seconds", startup.CacheTTLS, newCfg.CacheTTLS)
	warnRestartRequired("enforce_context", startup.EnforceContext, newCfg.EnforceContext)
	warnRestartRequired("precise_context", startup.PreciseContext, newCfg.PreciseContext)
	warnRestartRequired("clamp_max_tokens", startup.ClampMaxTokens, newCfg.ClampMaxTokens)
	warnRestartRequired("retry_max", startup.RetryMax, newCfg.RetryMax)
	warnRestartRequired("retry_backoff_ms", startup.RetryBackoffMs, newCfg.RetryBackoffMs)
	warnRestartRequired("coalesce", startup.Coalesce, newCfg.Coalesce)

	// Per-model settings outside backend.ModelSpec (cost rates, guardrail
	// overrides, request timeout, model-level fallbacks): scheduler.Reload only
	// evicts/reloads a model whose ModelSpec changed, so these can silently
	// drift from the file even for a model that IS otherwise reloaded correctly.
	old := make(map[string]config.Model, len(startup.Models))
	for _, m := range startup.Models {
		old[m.ID] = m
	}
	for _, m := range newCfg.Models {
		if prev, ok := old[m.ID]; ok && modelExtrasChanged(prev, m) {
			fmt.Fprintf(os.Stderr, "reload: model %q per-model overrides (cost/guardrail/clamp/timeout/fallbacks) changed; requires a restart to take effect; ignoring\n", m.ID)
		}
	}
}

// lintStrings renders config lint warnings for GET /admin/config, keeping
// internal/server independent of internal/config's LintWarning type.
func lintStrings(warnings []config.LintWarning) []string {
	out := make([]string, len(warnings))
	for i, w := range warnings {
		out[i] = w.String()
	}
	return out
}

// warnRestartRequired logs a restart-required line when oldV != newV. Values
// are compared via their default fmt formatting, which is exact for the
// plain scalar config fields this is used for (string/int/bool).
func warnRestartRequired(field string, oldV, newV any) {
	if fmt.Sprintf("%v", oldV) != fmt.Sprintf("%v", newV) {
		fmt.Fprintf(os.Stderr, "reload: %s changed (%v -> %v) requires a restart; ignoring\n", field, oldV, newV)
	}
}

// modelExtrasChanged reports whether any per-model setting NOT already covered
// by backend.ModelSpec (and thus not already handled by scheduler.Reload)
// differs between two Model entries for the same id.
func modelExtrasChanged(a, b config.Model) bool {
	if a.TimeoutS != b.TimeoutS || a.InputUSDPerMTok != b.InputUSDPerMTok || a.OutputUSDPerMTok != b.OutputUSDPerMTok {
		return true
	}
	if boolPtrDiffers(a.EnforceContext, b.EnforceContext) ||
		boolPtrDiffers(a.ClampMaxTokens, b.ClampMaxTokens) ||
		boolPtrDiffers(a.PreciseContext, b.PreciseContext) {
		return true
	}
	return !slices.Equal(a.ModelFallbacks, b.ModelFallbacks)
}

func boolPtrDiffers(a, b *bool) bool {
	if (a == nil) != (b == nil) {
		return true
	}
	return a != nil && *a != *b
}

// adoptBackendNames are the detect-and-adopt backends that can enumerate their
// own models for auto-discovery.
var adoptBackendNames = []string{"ollama", "lmstudio", "llamafile", "gpt4all"}

// discoverModels queries each present backend that implements backend.ModelLister
// and returns specs for models not already configured (configured wins on an id
// clash). A backend that fails to list is skipped with a warning.
func discoverModels(ctx context.Context, backends map[string]backend.Backend, existing []backend.ModelSpec) []backend.ModelSpec {
	have := make(map[string]bool, len(existing))
	for _, sp := range existing {
		have[sp.ID] = true
	}
	var found []backend.ModelSpec
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic discovery order
	for _, name := range names {
		lister, ok := backends[name].(backend.ModelLister)
		if !ok {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ids, err := lister.ListModels(lctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover: %s: %v\n", name, err)
			continue
		}
		for _, id := range ids {
			if id == "" || have[id] {
				continue
			}
			have[id] = true
			found = append(found, backend.ModelSpec{ID: id, Backend: name})
			fmt.Printf("discovered model %q on %s\n", id, name)
		}
	}
	return found
}

// allBackends constructs every known backend adapter (for detection/listing).
func allBackends(cfg config.Config) []backend.Backend {
	return []backend.Backend{
		llamacpp.New(cfg.LlamaServerPath),
		ollama.New(cfg.OllamaHost),
		mlx.New(cfg.MLXPython),
		lmstudio.New(cfg.LMStudioHost),
		openaiadopt.New("llamafile", orDefault(cfg.LlamafileHost, "http://127.0.0.1:8080"), "start it with `./model.llamafile --server`"),
		openaiadopt.New("gpt4all", orDefault(cfg.GPT4AllHost, "http://127.0.0.1:4891"), "enable the API server in GPT4All settings"),
	}
}

// newBackendByName constructs a backend by name, applying config (incl. a
// managed-install path preference for llamacpp).
func newBackendByName(name string, cfg config.Config) (backend.Backend, error) {
	switch name {
	case "llamacpp":
		binPath := cfg.LlamaServerPath
		if binPath == "" {
			if managed, ok := install.ManagedPath("llamacpp"); ok {
				binPath = managed // prefer a managed install over PATH
			}
		}
		return llamacpp.New(binPath), nil
	case "ollama":
		return ollama.New(cfg.OllamaHost), nil
	case "mlx":
		return mlx.New(cfg.MLXPython), nil
	case "lmstudio":
		return lmstudio.New(cfg.LMStudioHost), nil
	case "llamafile":
		return openaiadopt.New("llamafile", orDefault(cfg.LlamafileHost, "http://127.0.0.1:8080"),
			"start it with `./model.llamafile --server --port 8080`"), nil
	case "gpt4all":
		return openaiadopt.New("gpt4all", orDefault(cfg.GPT4AllHost, "http://127.0.0.1:4891"),
			"enable the API server in GPT4All → Settings → Application"), nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want llamacpp|ollama|mlx|lmstudio|llamafile|gpt4all)", name)
	}
}

// orDefault returns v if non-empty, else def.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// configureAccessLog wires the server's structured access log from the config
// value: "" disables it, "stderr"/"stdout" stream to those, any other value is a
// file path (parent dirs created). It returns a close func for the file (nil
// otherwise) so the caller can defer cleanup.
func configureAccessLog(srv *server.Server, dest string) (func(), error) {
	switch dest {
	case "":
		return nil, nil
	case "stderr":
		srv.SetAccessLog(os.Stderr)
		return nil, nil
	case "stdout":
		srv.SetAccessLog(os.Stdout)
		return nil, nil
	default:
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		srv.SetAccessLog(f)
		return func() { _ = f.Close() }, nil
	}
}

// defaultBackendName resolves the fallback backend for models that don't set one.
func defaultBackendName(cfg config.Config) string {
	if cfg.Backend != "" {
		return cfg.Backend
	}
	return "llamacpp"
}

// preloadIDs returns the ids of models configured to load at startup.
func preloadIDs(cfg config.Config) []string {
	var ids []string
	for _, m := range cfg.Models {
		if m.Preload {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// buildAuth constructs the authenticator from config: explicit tenants take
// precedence over the flat api_keys list.
func buildAuth(cfg config.Config) *auth.Authenticator {
	return auth.NewTenants(authTenants(cfg))
}

// parseModelFlag parses "id=/path/to/weights.gguf".
func parseModelFlag(s string) (config.Model, error) {
	id, path, ok := strings.Cut(s, "=")
	id, path = strings.TrimSpace(id), strings.TrimSpace(path)
	if !ok || id == "" || path == "" {
		return config.Model{}, fmt.Errorf("invalid --model %q: want id=/path/to/weights.gguf", s)
	}
	return config.Model{ID: id, Path: path}, nil
}

// configPath returns the --config value (persistent flag), or "" for the default.
func configPath(cmd *cobra.Command) string {
	if f := cmd.Flags().Lookup("config"); f != nil {
		return f.Value.String()
	}
	return ""
}
