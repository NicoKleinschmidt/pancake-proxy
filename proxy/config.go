package proxy

import (
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection/grpc_reflection_v1"
)

type UpstreamConfig struct {
	Address            string `mapstructure:"address"`
	Plaintext          bool   `mapstructure:"plaintext"`
	InsecureSkipVerify bool   `mapstructure:"insecureSkipVerify"`
}

type ServerConfig struct {
	Upstreams             []UpstreamConfig `mapstructure:"servers"`
	ServiceUpdateInterval time.Duration    `mapstructure:"serviceUpdateInterval"`

	// DisableReflection will not expose the
	DisableReflection bool `mapstructure:"disableReflection"`

	Logger *zap.Logger
}

func NewServer(config ServerConfig) *proxy {
	p := &proxy{
		services:                 make(map[string]*upstreamService),
		servicesMutex:            &sync.RWMutex{},
		serviceUpdateInterval:    config.ServiceUpdateInterval,
		upstreams:                upstreamConfig(config.Upstreams),
		internalServer:           grpc.NewServer(),
		logger:                   config.Logger,
		disableReflectionService: config.DisableReflection,
	}

	if p.logger == nil {
		p.logger = zap.NewNop()
	}

	grpc_reflection_v1.RegisterServerReflectionServer(p.internalServer, p)
	// grpc_reflection_v1alpha.RegisterServerReflectionServer(p.internalServer, reflection.AsV1Alpha(p))

	return p
}

func upstreamConfig(upstreams []UpstreamConfig) []*serverInfo {
	servers := make([]*serverInfo, len(upstreams))
	for i, config := range upstreams {
		servers[i] = &serverInfo{
			host:               config.Address,
			plaintext:          config.Plaintext,
			insecureSkipVerify: config.InsecureSkipVerify,
		}
	}
	return servers
}
