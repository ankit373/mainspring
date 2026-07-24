// Command mainspring is a backend-agnostic local inference server: one stable
// OpenAI-compatible API in front, pluggable engines (llama.cpp today) behind it.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
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
	root.AddCommand(cmdServe(), cmdBackends(), cmdModels(), cmdInstall(), cmdVersion())

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

			backends := []backend.Backend{
				llamacpp.New(cfg.LlamaServerPath),
				ollama.New(cfg.OllamaHost),
				mlx.New(cfg.MLXPython),
				lmstudio.New(cfg.LMStudioHost),
			}
			fmt.Printf("%-12s %-9s %-9s %s\n", "BACKEND", "PRESENT", "SOURCE", "DETAIL")
			for _, b := range backends {
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
			fmt.Println("\ndetect-and-adopt-only (planned): llamafile, gpt4all")
			return nil
		},
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
		addr         string
		modelFlags   []string
		apiKeys      []string
		keepAlive    int
		maxLoaded    int
		maxResident  int
		maxInflight  int
		maxQueue     int
		usageLedger  string
		backendName  string
		ollamaHost   string
		llamaServer  string
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
			for _, mf := range modelFlags {
				m, err := parseModelFlag(mf)
				if err != nil {
					return err
				}
				cfg.Models = append(cfg.Models, m)
			}
			if len(cfg.Models) == 0 {
				return fmt.Errorf("no models configured — pass --model id=/path/to/weights.gguf or add models to the config")
			}

			return runServe(cmd.Context(), cfg)
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
	return cmd
}

func runServe(ctx context.Context, cfg config.Config) error {
	fallback := defaultBackendName(cfg)
	specs := make([]backend.ModelSpec, 0, len(cfg.Models))
	needed := map[string]bool{}
	for _, m := range cfg.Models {
		bname := m.Backend
		if bname == "" {
			bname = fallback
		}
		needed[bname] = true
		specs = append(specs, backend.ModelSpec{
			ID:        m.ID,
			Backend:   bname,
			Path:      m.Path,
			CtxSize:   m.Ctx,
			GPULayers: m.GPULayers,
			ExtraArgs: m.Args,
		})
	}

	// Construct and detect only the backends the configured models actually use.
	backends := make(map[string]backend.Backend, len(needed))
	for name := range needed {
		be, err := newBackendByName(name, cfg)
		if err != nil {
			return err
		}
		if av := be.Detect(ctx); !av.Present {
			return fmt.Errorf("%s backend unavailable: %s", name, av.Reason)
		}
		backends[name] = be
	}

	sched := scheduler.New(backends, specs, scheduler.Options{
		KeepAlive: cfg.KeepAlive(),
		MaxLoaded: cfg.MaxLoaded,
		MaxBytes:  cfg.MaxBytes(),
	})
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

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Println(build.String())
	fmt.Printf("serving %d model(s) on %s\n", len(specs), cfg.Addr)
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

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Println("\nshutting down — reclaiming loaded models…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		sched.Shutdown(shutCtx)
		return nil
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
	default:
		return nil, fmt.Errorf("unknown backend %q (want llamacpp|ollama|mlx|lmstudio)", name)
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
		return auth.NewTenants(ts)
	}
	return auth.New(cfg.APIKeys)
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
