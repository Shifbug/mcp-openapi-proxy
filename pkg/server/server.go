package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rendis/mcp-openapi-proxy/pkg/auth"
	"github.com/rendis/mcp-openapi-proxy/pkg/client"
	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

// Config controls runtime behavior for the MCP proxy.
type Config struct {
	SpecSource         string
	BaseURL            string
	ToolPrefix         string
	ExcludeDeprecated  bool
	AllowInsecureHTTP  bool
	MaxBodyBytes       int64
	AuthProfile        string
	SkipSpecValidation bool
}

// Run loads the spec, generates tools, and starts the MCP stdio server.
func Run(cfg Config, extraHeaders map[string]string) error {
	endpoints, _, err := spec.LoadSpec(cfg.SpecSource, !cfg.SkipSpecValidation)
	if err != nil {
		return fmt.Errorf("load spec: %w", err)
	}

	httpClient := client.New(extraHeaders, cfg.MaxBodyBytes)
	authResolver := auth.NewResolver(cfg.AuthProfile)

	srv := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-openapi-proxy",
			Version: "0.1.0",
		},
		nil,
	)

	GenerateTools(srv, endpoints, httpClient, authResolver, cfg)

	indexedEndpointCount := 0
	for _, ep := range endpoints {
		if cfg.ExcludeDeprecated && ep.Deprecated {
			continue
		}
		indexedEndpointCount++
	}
	fmt.Fprintf(os.Stderr, "mcp-openapi-proxy: registered 3 tools for %d indexed endpoints from %s\n", indexedEndpointCount, cfg.SpecSource)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Printf("MCP server error: %v", err)
		return err
	}
	return nil
}
