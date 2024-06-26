package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NicoKleinschmidt/pancake-proxy/utils"
	"github.com/jhump/protoreflect/grpcreflect"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type serverInfo struct {
	host               string
	plaintext          bool
	insecureSkipVerify bool
}

type upstreamService struct {
	servers []*serverInfo
	next    atomic.Uint32
}

type proxy struct {
	upstreams []*serverInfo

	services      map[string]*upstreamService
	servicesMutex *sync.RWMutex

	internalServer *grpc.Server
	logger         *zap.Logger

	serviceUpdateInterval    time.Duration
	disableReflectionService bool
}

// RunBackgroundLoop MUST be called before the first request can be served.
// The function will block until the context is cancelled.
// This function will always return a non nil error.
func (p *proxy) RunProxy(ctx context.Context) error {
	ticker := time.NewTicker(p.serviceUpdateInterval)
	defer ticker.Stop()

	p.updateServices()

	for {
		select {
		case <-ticker.C:
			p.updateServices()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serviceName, ok := p.getTargetService(r)
	if !ok {
		w.WriteHeader(400)
		fmt.Fprint(w, "malformed request url")
		return
	}

	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")

	if p.handleReflection(w, r, serviceName) {
		return
	}

	server, ok := p.findServer(serviceName)
	if !ok {
		writeGrpcStatus(w, codes.Unimplemented, "no server provides the service")
		return
	}

	p.forwardRequest(r, w, server)
}

// findServer finds a server implementing the specified service using round robin load balancing.
func (p *proxy) findServer(serviceName string) (*serverInfo, bool) {
	p.servicesMutex.RLock()
	defer p.servicesMutex.RUnlock()

	service, ok := p.services[serviceName]
	if !ok || len(service.servers) == 0 {
		return nil, false
	}

	next := int(service.next.Add(1))
	server := service.servers[next%len(service.servers)]
	return server, true
}

// getTargetService returns the name of the service this request is targeting.
func (p *proxy) getTargetService(r *http.Request) (name string, ok bool) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	split := strings.SplitN(path, "/", 2)
	if len(split) != 2 {
		return "", false
	}

	return split[0], true
}

// forwardRequest forwards an incoming gRPC request to the specified server.
func (p *proxy) forwardRequest(req *http.Request, w http.ResponseWriter, server *serverInfo) {
	client := &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	req.URL.Host = server.host
	req.Host = server.host
	req.RequestURI = ""

	if server.plaintext {
		req.URL.Scheme = "http"
	} else {
		req.URL.Scheme = "https"
	}

	response, err := client.Do(req)
	if err != nil {
		p.logger.Debug("Failed to start request", zap.Error(err))
		return
	}

	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(response.StatusCode)
	if response.StatusCode != 200 {
		p.logger.Debug("received bad status", zap.Int("status_code", response.StatusCode))
		return
	}

	if _, err := io.Copy(utils.HttpAutoFlusher(w), response.Body); err != nil {
		p.logger.Debug("Request cancelled", zap.Error(err))
		return
	}

	for key, values := range response.Trailer {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

func (p *proxy) updateServices() {
	type result struct {
		server   *serverInfo
		services []string
	}

	var results []result

	upstreams := p.upstreams
	wg := sync.WaitGroup{}
	wg.Add(len(upstreams))

	p.logger.Debug("Updating upstream servers", zap.Int("count", len(upstreams)))

	for _, server := range upstreams {
		go func(server *serverInfo) {
			defer wg.Done()

			conn, err := grpc.NewClient(server.host, server.dialOptions()...)
			if err != nil {
				p.logger.Error("Failed to dial service", zap.String("target_host", server.host), zap.Error(err))
				return
			}

			client := grpcreflect.NewClientAuto(context.Background(), conn)
			services, err := client.ListServices()
			if err != nil {
				p.logger.Error("Failed to query services", zap.String("target_host", server.host), zap.Error(err))
				return
			}

			results = append(results, result{
				server:   server,
				services: services,
			})
		}(server)
	}

	wg.Wait()

	p.servicesMutex.Lock()
	defer p.servicesMutex.Unlock()

	for k := range p.services {
		delete(p.services, k)
	}

	for _, result := range results {
		for _, serviceName := range result.services {
			service := p.services[serviceName]
			if service == nil {
				service = &upstreamService{}
				p.services[serviceName] = service
			}

			service.servers = append(service.servers, result.server)
		}
	}
}

func writeGrpcStatus(w http.ResponseWriter, code codes.Code, msg string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.WriteHeader(200)
	w.Header().Add("Grpc-Status", strconv.Itoa(int(code)))
	w.Header().Add("Grpc-Message", msg)
}

func (server *serverInfo) dialOptions() []grpc.DialOption {
	if server.plaintext {
		return []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
	}

	return []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: server.insecureSkipVerify})),
	}
}
