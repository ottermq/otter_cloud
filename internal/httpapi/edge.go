package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/instances"
)

// The edge's names for what its static settings define (deploy/vps).
const (
	edgeEntryPoint = "postgres"
	// edgeTransportPrefix names a cluster's transport, which re-encrypts
	// to its PgBouncer, which requires TLS from its clients, and accepts
	// the cluster's own certificate and no other (#171). The hop runs
	// inside the node's WireGuard tunnel besides, which only that node can
	// end.
	edgeTransportPrefix = "pgbouncer-"
	// edgeTLSOptions offers ALPN "postgresql", which libpq 17+ and pgx
	// require in direct TLS and libpq 17+ offers even after an SSLRequest:
	// Traefik's default options offer only HTTP's, and Go refuses a
	// handshake with no protocol in common (#23).
	edgeTLSOptions = "postgres@file"
)

// EdgeHandler serves the edge's database routes (#103) in the format of
// Traefik's HTTP provider, which polls it over the tunnel: a TCP router
// per running database, by SNI, to its cluster's PgBouncer, and nothing
// else. It is served on its own listener (OTTER_EDGE_ADDR), and only to
// the trusted proxies, the edge's addresses.
func (s *Server) EdgeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /traefik", s.handleEdgeRoutes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !s.trusted(ap.Addr().Unmap()) {
			s.log.WarnContext(r.Context(), "edge routes refused", "remote_addr", r.RemoteAddr)
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type edgeConfig struct {
	TCP *edgeTCP `json:"tcp,omitempty"`
}

type edgeTCP struct {
	Routers           map[string]edgeRouter    `json:"routers"`
	Services          map[string]edgeService   `json:"services"`
	ServersTransports map[string]edgeTransport `json:"serversTransports"`
}

type edgeRouter struct {
	EntryPoints []string `json:"entryPoints"`
	Rule        string   `json:"rule"`
	Service     string   `json:"service"`
	// TLS is terminated at the edge, with the databases' wildcard: the
	// default certificate in the edge's static settings.
	TLS edgeRouterTLS `json:"tls"`
}

type edgeRouterTLS struct {
	Options string `json:"options"`
}

type edgeService struct {
	LoadBalancer struct {
		ServersTransport string       `json:"serversTransport"`
		Servers          []edgeServer `json:"servers"`
	} `json:"loadBalancer"`
}

type edgeServer struct {
	Address string `json:"address"`
	TLS     bool   `json:"tls"`
}

type edgeTransport struct {
	TLS edgeTransportTLS `json:"tls"`
}

// edgeTransportTLS pins the cluster's certificate: self-signed, it is the
// only root, and serverName is its subjectAltName. Traefik takes rootCAs
// as the PEM itself, as well as a file's path.
type edgeTransportTLS struct {
	ServerName string   `json:"serverName"`
	RootCAs    []string `json:"rootCAs"`
}

func (s *Server) handleEdgeRoutes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cfg, err := edgeRoutes(ctx, s.db)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// edgeRoutes is a route for each database a customer can connect to: one
// that runs, has a hostname, and isn't blocked over its storage (its
// PgBouncer entry is gone then, #99). A service per cluster: its PgBouncer,
// over a transport that pins the cluster's certificate. A cluster whose
// certificate the worker hasn't recorded yet, or that the edge can't
// verify, has no routes: the worker reports its databases degraded.
func edgeRoutes(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (edgeConfig, error) {
	rows, err := db.Query(ctx, `
		SELECT i.infra_name, i.endpoint_host, c.name, host(c.pool_addr), c.pool_port, c.server_cert
		FROM instances i JOIN clusters c ON c.id = i.cluster_id
		WHERE i.status = 'running' AND i.storage_state <> 'blocked' AND i.endpoint_host IS NOT NULL
			AND c.server_cert IS NOT NULL
		ORDER BY i.infra_name`)
	if err != nil {
		return edgeConfig{}, fmt.Errorf("list the routes: %w", err)
	}
	defer rows.Close()
	tcp := &edgeTCP{Routers: map[string]edgeRouter{}, Services: map[string]edgeService{},
		ServersTransports: map[string]edgeTransport{}}
	for rows.Next() {
		var infra, host, cluster, poolAddr, cert string
		var poolPort int
		if err := rows.Scan(&infra, &host, &cluster, &poolAddr, &poolPort, &cert); err != nil {
			return edgeConfig{}, fmt.Errorf("list the routes: %w", err)
		}
		if _, ok := tcp.Services[cluster]; !ok {
			name, err := instances.ClusterCertName([]byte(cert))
			if err != nil {
				continue
			}
			var svc edgeService
			svc.LoadBalancer.ServersTransport = edgeTransportPrefix + cluster
			svc.LoadBalancer.Servers = []edgeServer{{Address: net.JoinHostPort(poolAddr, strconv.Itoa(poolPort)), TLS: true}}
			tcp.Services[cluster] = svc
			tcp.ServersTransports[edgeTransportPrefix+cluster] = edgeTransport{
				TLS: edgeTransportTLS{ServerName: name, RootCAs: []string{cert}}}
		}
		tcp.Routers[infra] = edgeRouter{EntryPoints: []string{edgeEntryPoint},
			Rule: "HostSNI(`" + host + "`)", Service: cluster, TLS: edgeRouterTLS{Options: edgeTLSOptions}}
	}
	if err := rows.Err(); err != nil {
		return edgeConfig{}, fmt.Errorf("list the routes: %w", err)
	}
	if len(tcp.Routers) == 0 {
		// Traefik takes an empty configuration; empty maps are noise in
		// its logs.
		return edgeConfig{}, nil
	}
	return edgeConfig{TCP: tcp}, nil
}
