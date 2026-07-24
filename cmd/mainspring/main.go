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
	"github.com/ankit373/mainspring/internal/build"
	"github.com/ankit373/mainspring/internal/config"
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
	root.AddCommand(cmdServe(), cmdBackends(), cmdModels(), cmdVersion())

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

			backends := []backend.Backend{llamacpp.New(cfg.LlamaServerPath)}
			fmt.Printf("%-12s %-9s %s\n", "BACKEND", "PRESENT", "DETAIL")
			for _, b := range backends {
				av := b.Detect(ctx)
				detail := av.Path
				if av.Version != "" {
					detail += "  " + av.Version
				}
				if !av.Present {
					detail = av.Reason
				}
				fmt.Printf("%-12s %-9v %s\n", av.Name, av.Present, detail)
			}
			fmt.Println("\nplanned: mlx (Apple Silicon), ollama (adopt existing daemon);")
			fmt.Println("detect-and-adopt-only: lmstudio, llamafile, gpt4all")
			return nil
		},
	}
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
			fmt.Printf("%-24s %-6s %s\n", "MODEL", "CTX", "PATH")
			for _, m := range cfg.Models {
				fmt.Printf("%-24s %-6d %s\n", m.ID, m.Ctx, m.Path)
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
		usageLedger  string
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
			if cmd.Flags().Changed("usage-ledger") {
				cfg.UsageLedger = usageLedger
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
	cmd.Flags().StringVar(&usageLedger, "usage-ledger", "", "JSONL usage ledger path (empty = default location, \"off\" = disable)")
	cmd.Flags().StringVar(&llamaServer, "llama-server", "", "path to llama-server (default: look up PATH)")
	return cmd
}

func runServe(ctx context.Context, cfg config.Config) error {
	specs := make([]backend.ModelSpec, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		specs = append(specs, backend.ModelSpec{
			ID:        m.ID,
			Path:      m.Path,
			CtxSize:   m.Ctx,
			GPULayers: m.GPULayers,
			ExtraArgs: m.Args,
		})
	}

	be := llamacpp.New(cfg.LlamaServerPath)
	if av := be.Detect(ctx); !av.Present {
		return fmt.Errorf("llama.cpp backend unavailable: %s", av.Reason)
	}

	sched := scheduler.New(be, specs, scheduler.Options{
		KeepAlive: cfg.KeepAlive(),
		MaxLoaded: cfg.MaxLoaded,
		MaxBytes:  cfg.MaxBytes(),
	})
	authn := auth.New(cfg.APIKeys)

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
