// Copyright 2018-2021 CERN
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
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package rgrpc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/cs3org/reva/internal/grpc/interceptors/appctx"
	"github.com/cs3org/reva/internal/grpc/interceptors/auth"
	"github.com/cs3org/reva/internal/grpc/interceptors/log"
	"github.com/cs3org/reva/internal/grpc/interceptors/recovery"
	"github.com/cs3org/reva/internal/grpc/interceptors/token"
	"github.com/cs3org/reva/internal/grpc/interceptors/useragent"
	"github.com/cs3org/reva/pkg/sharedconf"
	rtrace "github.com/cs3org/reva/pkg/trace"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/johanbrandhorst/certify"
	"github.com/johanbrandhorst/certify/issuers/vault"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

// UnaryInterceptors is a map of registered unary grpc interceptors.
var UnaryInterceptors = map[string]NewUnaryInterceptor{}

// StreamInterceptors is a map of registered streaming grpc interceptor
var StreamInterceptors = map[string]NewStreamInterceptor{}

// NewUnaryInterceptor is the type that unary interceptors need to register.
type NewUnaryInterceptor func(m map[string]interface{}) (grpc.UnaryServerInterceptor, int, error)

// NewStreamInterceptor is the type that stream interceptors need to register.
type NewStreamInterceptor func(m map[string]interface{}) (grpc.StreamServerInterceptor, int, error)

// RegisterUnaryInterceptor registers a new unary interceptor.
func RegisterUnaryInterceptor(name string, newFunc NewUnaryInterceptor) {
	UnaryInterceptors[name] = newFunc
}

// RegisterStreamInterceptor registers a new stream interceptor.
func RegisterStreamInterceptor(name string, newFunc NewStreamInterceptor) {
	StreamInterceptors[name] = newFunc
}

// Services is a map of service name and its new function.
var Services = map[string]NewService{}

// Register registers a new gRPC service with name and new function.
func Register(name string, newFunc NewService) {
	Services[name] = newFunc
}

// NewService is the function that gRPC services need to register at init time.
// It returns an io.Closer to close the service and a list of service endpoints that need to be unprotected.
type NewService func(conf map[string]interface{}, ss *grpc.Server) (Service, error)

// Service represents a grpc service.
type Service interface {
	Register(ss *grpc.Server)
	io.Closer
	UnprotectedEndpoints() []string
}

type unaryInterceptorTriple struct {
	Name        string
	Priority    int
	Interceptor grpc.UnaryServerInterceptor
}

type streamInterceptorTriple struct {
	Name        string
	Priority    int
	Interceptor grpc.StreamServerInterceptor
}

type RSA struct {
	bits int
}

type config struct {
	Network          string                            `mapstructure:"network"`
	Address          string                            `mapstructure:"address"`
	ShutdownDeadline int                               `mapstructure:"shutdown_deadline"`
	Services         map[string]map[string]interface{} `mapstructure:"services"`
	Interceptors     map[string]map[string]interface{} `mapstructure:"interceptors"`
	EnableReflection bool                              `mapstructure:"enable_reflection"`

	Insecure   bool `mapstructure:"insecure"`
	SkipVerify bool `mapstructure:"SkipVerify"`

	CertFile       string `mapstructure:"certfile"`
	KeyFile        string `mapstructure:"keyfile"`
	CACertFile     string `mapstructure:"ca_certfile"`
	CertifyCA      bool   `mapstructure:"certifyca"`
	SelfSignedCert bool   `mapstructure:"self_signed_cert"`

	VaultScheme   string `mapstructure:"vault_scheme"`
	VaultHost     string `mapstructure:"vault_host"`
	VaultToken    string `mapstructure:"vault_token"`
	VaultRole     string `mapstructure:"vault_role"`
	VaultCertFile string `mapstructure:"vault_certfile"`
}

func (c *config) init() {
	if c.Network == "" {
		c.Network = "tcp"
	}

	if c.Address == "" {
		c.Address = sharedconf.GetGatewaySVC("0.0.0.0:19000")
	}

	if c.CertFile == "" {
		c.CertFile = "/home/amal/vault/vault.pem"
	}
	if c.KeyFile == "" {
		c.KeyFile = "/home/amal/vault/vault.key"
	}
	if c.CACertFile == "" {
		c.CACertFile = "/home/amal/Documents/gh/reva/cert/ca-cert.pem"
	}
	if c.VaultHost == "" {
		c.VaultHost = "localhost:8200"
	}
	if c.VaultRole == "" {
		c.VaultRole = "my-role"
	}
	if c.VaultScheme == "" {
		c.VaultScheme = "https"
	}
	if c.VaultCertFile == "" {
		c.VaultCertFile = "/home/amal/Documents/gh/reva/ca-vault.cert"
	}

	sharedconf.SetInsecure(c.Insecure)
	sharedconf.SetSkipVerify(c.SkipVerify)
}

// Server is a gRPC server.
type Server struct {
	s        *grpc.Server
	conf     *config
	listener net.Listener
	log      zerolog.Logger
	services map[string]Service
}

// NewServer returns a new Server.
func NewServer(m interface{}, log zerolog.Logger) (*Server, error) {
	conf := &config{}
	if err := mapstructure.Decode(m, conf); err != nil {
		return nil, err
	}

	conf.init()

	server := &Server{conf: conf, log: log, services: map[string]Service{}}

	return server, nil
}

// Start starts the server.
func (s *Server) Start(ln net.Listener) error {
	if err := s.registerServices(); err != nil {
		err = errors.Wrap(err, "unable to register services")
		return err
	}

	s.listener = ln
	s.log.Info().Msgf("grpc server listening at %s:%s", s.Network(), s.Address())
	err := s.s.Serve(s.listener)
	if err != nil {
		err = errors.Wrap(err, "serve failed")
		return err
	}
	return nil
}

func (s *Server) isInterceptorEnabled(name string) bool {
	for k := range s.conf.Interceptors {
		if k == name {
			return true
		}
	}
	return false
}

func (s *Server) isServiceEnabled(svcName string) bool {
	for key := range Services {
		if key == svcName {
			return true
		}
	}
	return false
}

func (s *Server) registerServices() error {
	for svcName := range s.conf.Services {
		if s.isServiceEnabled(svcName) {
			newFunc := Services[svcName]
			svc, err := newFunc(s.conf.Services[svcName], s.s)
			if err != nil {
				return errors.Wrapf(err, "rgrpc: grpc service %s could not be started,", svcName)
			}
			s.services[svcName] = svc
			s.log.Info().Msgf("rgrpc: grpc service enabled: %s", svcName)
		} else {
			message := fmt.Sprintf("rgrpc: grpc service %s does not exist", svcName)
			return errors.New(message)
		}
	}

	// obtain list of unprotected endpoints
	unprotected := []string{}
	for _, svc := range s.services {
		unprotected = append(unprotected, svc.UnprotectedEndpoints()...)
	}

	opts, err := s.getInterceptors(unprotected)
	if err != nil {
		return err
	}

	creds, err := getCredentials(s)
	if err != nil {
		return err
	}
	opts = append(opts, grpc.Creds(creds))

	grpcServer := grpc.NewServer(opts...)

	for _, svc := range s.services {
		svc.Register(grpcServer)
	}

	if s.conf.EnableReflection {
		s.log.Info().Msg("rgrpc: grpc server reflection enabled")
		reflection.Register(grpcServer)
	}

	s.s = grpcServer

	return nil
}

// TODO(labkode): make closing with deadline.
func (s *Server) cleanupServices() {
	for name, svc := range s.services {
		if err := svc.Close(); err != nil {
			s.log.Error().Err(err).Msgf("error closing service %q", name)
		} else {
			s.log.Info().Msgf("service %q correctly closed", name)
		}
	}
}

// Stop stops the server.
func (s *Server) Stop() error {
	s.cleanupServices()
	s.s.Stop()
	return nil
}

// GracefulStop gracefully stops the server.
func (s *Server) GracefulStop() error {
	s.cleanupServices()
	s.s.GracefulStop()
	return nil
}

// Network returns the network type.
func (s *Server) Network() string {
	return s.conf.Network
}

// Address returns the network address.
func (s *Server) Address() string {
	return s.conf.Address
}

func (s *Server) getInterceptors(unprotected []string) ([]grpc.ServerOption, error) {
	unaryTriples := []*unaryInterceptorTriple{}
	for name, newFunc := range UnaryInterceptors {
		if s.isInterceptorEnabled(name) {
			inter, prio, err := newFunc(s.conf.Interceptors[name])
			if err != nil {
				err = errors.Wrapf(err, "rgrpc: error creating unary interceptor: %s,", name)
				return nil, err
			}
			triple := &unaryInterceptorTriple{
				Name:        name,
				Priority:    prio,
				Interceptor: inter,
			}
			unaryTriples = append(unaryTriples, triple)
		}
	}

	// sort unary triples
	sort.SliceStable(unaryTriples, func(i, j int) bool {
		return unaryTriples[i].Priority < unaryTriples[j].Priority
	})

	authUnary, err := auth.NewUnary(s.conf.Interceptors["auth"], unprotected)
	if err != nil {
		return nil, errors.Wrap(err, "rgrpc: error creating unary auth interceptor")
	}

	unaryInterceptors := []grpc.UnaryServerInterceptor{authUnary}
	for _, t := range unaryTriples {
		unaryInterceptors = append(unaryInterceptors, t.Interceptor)
		s.log.Info().Msgf("rgrpc: chaining grpc unary interceptor %s with priority %d", t.Name, t.Priority)
	}

	unaryInterceptors = append(unaryInterceptors,
		otelgrpc.UnaryServerInterceptor(
			otelgrpc.WithTracerProvider(rtrace.Provider),
			otelgrpc.WithPropagators(rtrace.Propagator)),
	)

	unaryInterceptors = append([]grpc.UnaryServerInterceptor{
		appctx.NewUnary(s.log),
		token.NewUnary(),
		useragent.NewUnary(),
		log.NewUnary(),
		recovery.NewUnary(),
	}, unaryInterceptors...)
	unaryChain := grpc_middleware.ChainUnaryServer(unaryInterceptors...)

	streamTriples := []*streamInterceptorTriple{}
	for name, newFunc := range StreamInterceptors {
		if s.isInterceptorEnabled(name) {
			inter, prio, err := newFunc(s.conf.Interceptors[name])
			if err != nil {
				err = errors.Wrapf(err, "rgrpc: error creating streaming interceptor: %s,", name)
				return nil, err
			}
			triple := &streamInterceptorTriple{
				Name:        name,
				Priority:    prio,
				Interceptor: inter,
			}
			streamTriples = append(streamTriples, triple)
		}
	}
	// sort stream triples
	sort.SliceStable(streamTriples, func(i, j int) bool {
		return streamTriples[i].Priority < streamTriples[j].Priority
	})

	authStream, err := auth.NewStream(s.conf.Interceptors["auth"], unprotected)
	if err != nil {
		return nil, errors.Wrap(err, "rgrpc: error creating stream auth interceptor")
	}

	streamInterceptors := []grpc.StreamServerInterceptor{authStream}
	for _, t := range streamTriples {
		streamInterceptors = append(streamInterceptors, t.Interceptor)
		s.log.Info().Msgf("rgrpc: chaining grpc streaming interceptor %s with priority %d", t.Name, t.Priority)
	}

	streamInterceptors = append([]grpc.StreamServerInterceptor{
		authStream,
		appctx.NewStream(s.log),
		token.NewStream(),
		useragent.NewStream(),
		log.NewStream(),
		recovery.NewStream(),
	}, streamInterceptors...)
	streamChain := grpc_middleware.ChainStreamServer(streamInterceptors...)

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(unaryChain),
		grpc.StreamInterceptor(streamChain),
	}

	return opts, nil
}

func certFilesExist(s *Server) bool {
	if _, err := os.Stat(s.conf.CertFile); errors.Is(err, os.ErrNotExist) {
		s.log.Error().Msg("certificate file doesn't exist at specified path.")
		return false
	}
	if _, err := os.Stat(s.conf.KeyFile); errors.Is(err, os.ErrNotExist) {
		s.log.Error().Msg("key file doesn't exist at specified path.")
		return false
	}
	return true
}

func getCredentials(s *Server) (credentials.TransportCredentials, error) {
	var selfSignedCert bool = certFilesExist(s)

	switch {
	// Certificates signed by Vault via Certify
	case selfSignedCert && s.conf.CertifyCA:
		s.log.Error().Msg("can't choose self-signed files and Certify at the same time")
		os.Exit(1)
	// Self-signed cetificates
	case selfSignedCert:
		creds, err := credentials.NewServerTLSFromFile(s.conf.CertFile, s.conf.KeyFile)
		if err != nil {
			s.log.Error().Msg("failed to setup TLS with local files")
			os.Exit(1)
		}
		return creds, nil
	case s.conf.CertifyCA:
		creds, err := vaultCert(s.conf)
		if err != nil {
			s.log.Error().Msg("failed to setup TLS with Certify")
			os.Exit(1)
		}
		return creds, nil
	// Insecure
	default:
		creds := insecure.NewCredentials()
		s.log.Info().Msg("Setting up insecure connection.")
		return creds, nil
	}
	return nil, errors.New("could not setup tls")
}

func (r RSA) Generate() (crypto.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, r.bits)
}

func vaultCert(conf *config) (credentials.TransportCredentials, error) {
	b, err := ioutil.ReadFile(conf.VaultCertFile)
	if err != nil {
		return nil, fmt.Errorf("vaultCert: problem with input file")
	}
	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("vaultCert: failed to append certificates")
	}
	issuer := &vault.Issuer{
		URL: &url.URL{
			Scheme: conf.VaultScheme,
			Host:   conf.VaultHost,
		},
		TLSConfig: &tls.Config{
			RootCAs: cp,
		},
		Token: conf.VaultToken,
		Role:  conf.VaultRole,
	}
	cfg := certify.CertConfig{
		SubjectAlternativeNames: []string{"localhost"},
		IPSubjectAlternativeNames: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
		KeyGenerator: RSA{bits: 2048},
	}
	c := &certify.Certify{
		CommonName:  "localhost",
		Issuer:      issuer,
		Cache:       certify.NewMemCache(),
		CertConfig:  &cfg,
		RenewBefore: 24 * time.Hour,
	}
	tlsConfig := &tls.Config{
		GetCertificate: c.GetCertificate,
	}
	return credentials.NewTLS(tlsConfig), nil
}
