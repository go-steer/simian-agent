// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-steer/simian-agent/pkg/webui"
)

// newWebCmd is the standalone UI: the controller's page with no controller
// behind it, for watching several Simians from one place. Nothing is
// proxied; the browser calls each controller directly, signed in to each,
// so every controller still checks who the user is and records them in its
// own audit trail.
func newWebCmd() *cobra.Command {
	var (
		addr        string
		controllers []string
	)
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the web UI alone, for watching several Simian controllers from one page",
		Long: `Serve Simian's web UI at /ui/ without a controller behind it: no Kubernetes,
no LLM. The page lists the controllers given with --controllers (and any the
user adds), and calls each from the browser. Each controller must list this
UI's origin in its --ui-allowed-origins (chart: ui.allowedOrigins), and the
user signs in to each once.`,
		Example: `  simian web --addr :8080 \
    --controllers simian-iap=https://simian.example.com \
    --controllers simian-2=https://simian-2.example.com`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := webui.ParseControllers(controllers)
			if err != nil {
				return fmt.Errorf("--controllers: %w", err)
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return serveWeb(ctx, addr, list, slog.New(slog.NewJSONHandler(os.Stdout, nil)))
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8080", "Listen address")
	cmd.Flags().StringSliceVar(&controllers, "controllers", nil, "Simians the page offers, as name=https://origin of each controller's UI, e.g. simian-2=https://simian-2.example.com (repeatable)")
	return cmd
}

// serveWeb serves the standalone UI on addr until ctx is done.
func serveWeb(ctx context.Context, addr string, list []webui.Controller, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: webui.StandaloneHandler(list), ReadHeaderTimeout: 5 * time.Second}
	logger.Info("simian web: standalone UI at /ui/", slog.String("addr", ln.Addr().String()), slog.Int("controllers", len(list)))
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
