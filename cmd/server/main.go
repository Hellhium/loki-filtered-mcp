// Command server is the loki-filtered-mcp entrypoint. It loads a single YAML
// config, builds one independent object graph per configured instance, and
// serves them all from one HTTP listener: MCP over Streamable HTTP at /mcp and
// the enforced Loki read API under /loki/api/v1/, with the caller's bearer
// token selecting which instance answers.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/instance"
	"github.com/Hellhium/loki-filtered-mcp/internal/router"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to the YAML config file (required)")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("missing required -config flag")
	}

	if err := run(*configPath); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	instances, err := instance.BuildAll(cfg.Resolved(), version)
	if err != nil {
		return err
	}

	handler, err := router.New(instances)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Serve in a goroutine so we can wait for a shutdown signal.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("loki-filtered-mcp %s listening on %s — %d instance(s), MCP at POST %s, Loki API under %s",
			version, cfg.Server.Listen, len(instances), router.MCPPath, router.ProxyPrefix)
		for _, in := range instances {
			// Tokens are config.Secret and redact themselves, but they are not
			// logged at all: an instance is identified by name.
			log.Printf("  instance %q: endpoints=%s, %d filter(s), on_conflict=%s, enforce_label_apis=%t, disclose_filters=%t, upstream=%s, org_id=%q",
				in.Name, in.Endpoints(), len(in.Config.Filters),
				in.Config.Enforcement.OnConflict, in.Config.Enforcement.EnforceLabelAPIs,
				in.Config.Enforcement.DiscloseFilters,
				in.Config.Loki.URL, in.Config.Loki.OrgID)
		}
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(ctx)
	}
}
