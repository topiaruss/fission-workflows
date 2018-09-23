package bundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/fission/fission-workflows/pkg/api"
	"github.com/fission/fission-workflows/pkg/api/aggregates"
	"github.com/fission/fission-workflows/pkg/api/store"
	"github.com/fission/fission-workflows/pkg/apiserver"
	"github.com/fission/fission-workflows/pkg/controller"
	"github.com/fission/fission-workflows/pkg/controller/expr"
	wfictr "github.com/fission/fission-workflows/pkg/controller/invocation"
	wfctr "github.com/fission/fission-workflows/pkg/controller/workflow"
	"github.com/fission/fission-workflows/pkg/fes"
	"github.com/fission/fission-workflows/pkg/fes/backend/mem"
	"github.com/fission/fission-workflows/pkg/fes/backend/nats"
	"github.com/fission/fission-workflows/pkg/fnenv"
	"github.com/fission/fission-workflows/pkg/fnenv/fission"
	"github.com/fission/fission-workflows/pkg/fnenv/native"
	"github.com/fission/fission-workflows/pkg/fnenv/native/builtin"
	"github.com/fission/fission-workflows/pkg/fnenv/workflows"
	"github.com/fission/fission-workflows/pkg/scheduler"
	"github.com/fission/fission-workflows/pkg/util"
	"github.com/fission/fission-workflows/pkg/util/labels"
	"github.com/fission/fission-workflows/pkg/util/pubsub"
	"github.com/fission/fission-workflows/pkg/version"
	controllerc "github.com/fission/fission/controller/client"
	executor "github.com/fission/fission/executor/client"
	"github.com/gorilla/handlers"
	"github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-prometheus"
	grpcruntime "github.com/grpc-ecosystem/grpc-gateway/runtime"
	grpc_opentracing "github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	jaegerlog "github.com/uber/jaeger-client-go/log"
	jaegerprom "github.com/uber/jaeger-lib/metrics/prometheus"
	"google.golang.org/grpc"
)

const (
	gRPCAddress             = ":5555"
	apiGatewayAddress       = ":8080"
	fissionProxyAddress     = ":8888"
	jaegerTracerServiceName = "fission.workflows"
)

type App struct {
	*Options
	closers map[string]io.Closer
}

func (app *App) RegisterCloser(name string, closer io.Closer) {
	if _, ok := app.closers[name]; ok {
		panic(fmt.Sprintf("duplicate registry for key %s", name))
	}

	app.closers[name] = closer
}

func (app *App) Close() error {
	var errorOccured bool
	for name, closer := range app.closers {
		err := closer.Close()
		if err != nil {
			log.Errorf("Error while closing %s: %v", name, err)
			errorOccured = true
		} else {
			log.Infof("Closed %s", name)
		}
	}
	if errorOccured {
		return errors.New("error(s) occurred while closing application")
	}
	return nil
}

type Options struct {
	Nats                 *nats.Config
	Fission              *FissionOptions
	InternalRuntime      bool
	InvocationController bool
	WorkflowController   bool
	AdminAPI             bool
	WorkflowAPI          bool
	HTTPGateway          bool
	InvocationAPI        bool
	Metrics              bool
	Debug                bool
}

type FissionOptions struct {
	ExecutorAddress string
	ControllerAddr  string
	RouterAddr      string
}

// Run serves enabled components in a blocking way
func Run(ctx context.Context, opts *Options) error {
	log.WithFields(log.Fields{
		"version": fmt.Sprintf("%+v", version.VersionInfo()),
		"config":  fmt.Sprintf("%+v", opts),
	}).Info("Starting bundle...")
	app := &App{
		Options: opts,
		closers: map[string]io.Closer{},
	}

	// See https://github.com/jaegertracing/jaeger-client-go for the env vars to set; defaults to local Jaeger
	// instance with default ports.
	cfg, err := jaegercfg.FromEnv()
	if err != nil {
		log.Fatalf("Failed to read Jaeger config from env: %v", err)
	}
	if opts.Debug {
		// Debug: do not sample down
		cfg.Sampler = &jaegercfg.SamplerConfig{
			Type:  jaeger.SamplerTypeConst,
			Param: 1,
		}
		cfg.Reporter = &jaegercfg.ReporterConfig{
			LogSpans: true,
		}
	}

	// Initialize tracer with a logger and a metrics factory
	closer, err := cfg.InitGlobalTracer(
		jaegerTracerServiceName,
		jaegercfg.Logger(jaegerlog.StdLogger),
		jaegercfg.Metrics(jaegerprom.New()),
	)
	if err != nil {
		log.Fatalf("Could not initialize jaeger tracer: %s", err.Error())
	}
	tracer := opentracing.GlobalTracer()
	defer closer.Close()
	log.Debugf("Configured Jaeger tracer '%s' (pushing traces to '%s')", jaegerTracerServiceName,
		cfg.Sampler.SamplingServerURL)

	var es fes.Backend
	var esPub pubsub.Publisher

	grpcServer := grpc.NewServer(
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpc_prometheus.StreamServerInterceptor,
			grpc_opentracing.OpenTracingStreamServerInterceptor(tracer),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_prometheus.UnaryServerInterceptor,
			grpc_opentracing.OpenTracingServerInterceptor(tracer),
		)),
	)

	//
	// Event Store
	//
	if opts.Nats != nil {
		log.WithFields(log.Fields{
			"url":     "!redacted!",
			"cluster": opts.Nats.Cluster,
			"client":  opts.Nats.Client,
		}).Infof("Using event store: NATS")
		natsEs := setupNatsEventStoreClient(opts.Nats.URL, opts.Nats.Cluster, opts.Nats.Client)
		es = natsEs
		esPub = natsEs
	} else {
		log.Warn("No event store provided; using the development, in-memory event store")
		backend := mem.NewBackend()
		es = backend
		esPub = backend
	}

	// Caches
	wfiCache := getInvocationStore(app, esPub)
	wfCache := getWorkflowStore(app, esPub)

	//
	// Function Runtimes
	//
	invocationAPI := api.NewInvocationAPI(es)
	resolvers := map[string]fnenv.RuntimeResolver{}
	runtimes := map[string]fnenv.Runtime{}

	if opts.InternalRuntime || opts.Fission != nil {
		log.Infof("Using Task Runtime: Workflow")
		reflectiveRuntime := workflows.NewRuntime(invocationAPI, wfiCache())
		runtimes[workflows.Name] = reflectiveRuntime
	} else {
		log.Info("No function runtimes specified.")
	}
	if opts.InternalRuntime {
		log.Infof("Using Task Runtime: Internal")
		internalRuntime := setupInternalFunctionRuntime()
		runtimes["internal"] = internalRuntime
		resolvers["internal"] = internalRuntime
		log.Infof("Internal runtime functions: %v", internalRuntime.Installed())
	}
	if opts.Fission != nil {
		log.WithFields(log.Fields{
			"controller": opts.Fission.ControllerAddr,
			"router":     opts.Fission.RouterAddr,
			"executor":   opts.Fission.ExecutorAddress,
		}).Infof("Using Task Runtime: Fission")
		runtimes["fission"] = setupFissionFunctionRuntime(opts.Fission.ExecutorAddress, opts.Fission.RouterAddr)
		resolvers["fission"] = setupFissionFunctionResolver(opts.Fission.ControllerAddr)
	}

	//
	// Controllers
	//
	if opts.InvocationController || opts.WorkflowController {
		var ctrls []controller.Controller
		if opts.WorkflowController {
			log.Info("Using controller: workflow")
			ctrls = append(ctrls, setupWorkflowController(wfCache(), es, resolvers))
		}

		if opts.InvocationController {
			log.Info("Using controller: invocation")
			ctrls = append(ctrls, setupInvocationController(wfiCache(), wfCache(), es, runtimes, resolvers))
		}

		ctrl := controller.NewMetaController(ctrls...)
		go ctrl.Run(ctx)
		defer func() {
			err := ctrl.Close()
			if err != nil {
				log.Errorf("Failed to stop controllers: %v", err)
			} else {
				log.Info("Stopped controllers")
			}
		}()
	} else {
		log.Info("No controllers specified to run.")
	}

	//
	// Fission integration
	//
	if opts.Fission != nil {
		proxyMux := http.NewServeMux()
		runFissionEnvironmentProxy(proxyMux, es, wfiCache(), wfCache(), resolvers)
		fissionProxySrv := &http.Server{Addr: fissionProxyAddress}
		fissionProxySrv.Handler = handlers.LoggingHandler(os.Stdout, proxyMux)

		if opts.Metrics {
			setupMetricsEndpoint(proxyMux)
		}

		go func() {
			err := fissionProxySrv.ListenAndServe()
			log.WithField("err", err).Info("Fission Proxy server stopped")
		}()
		defer func() {
			err := fissionProxySrv.Shutdown(ctx)
			if err != nil {
				log.Errorf("Failed to stop Fission Proxy server: %v", err)
			} else {
				log.Info("Stopped Fission Proxy server")
			}
		}()
		log.Info("Serving HTTP Fission Proxy at: ", fissionProxySrv.Addr)
	}

	//
	// gRPC API
	//
	if opts.AdminAPI {
		serveAdminAPI(grpcServer)
	}

	if opts.WorkflowAPI {
		serveWorkflowAPI(grpcServer, es, resolvers, wfCache())
	}

	if opts.InvocationAPI {
		serveInvocationAPI(grpcServer, es, wfiCache())
	}

	if opts.AdminAPI || opts.WorkflowAPI || opts.InvocationAPI {
		if opts.Metrics {
			log.Debug("Instrumenting gRPC server with Prometheus metrics")
			grpc_prometheus.Register(grpcServer)
		}

		lis, err := net.Listen("tcp", gRPCAddress)
		if err != nil {
			log.Fatalf("failed to listen: %v", err)
		}
		go grpcServer.Serve(lis)
		defer func() {
			grpcServer.GracefulStop()
			lis.Close()
			log.Info("Stopped gRPC server")
		}()
		log.Info("Serving gRPC services at: ", lis.Addr())
	}

	//
	// HTTP API
	//
	if opts.HTTPGateway || opts.Metrics {
		grpcMux := grpcruntime.NewServeMux()
		httpMux := http.NewServeMux()

		if opts.HTTPGateway {

			var admin, wf, wfi string
			if opts.AdminAPI {
				admin = gRPCAddress
			}
			if opts.WorkflowAPI {
				wf = gRPCAddress
			}
			if opts.InvocationAPI {
				wfi = gRPCAddress
			}
			serveHTTPGateway(ctx, grpcMux, admin, wf, wfi)
		}

		if opts.Metrics {
			setupMetricsEndpoint(httpMux)
			log.Infof("Set up prometheus collector: %v/metrics", apiGatewayAddress)
		}

		httpApiSrv := &http.Server{Addr: apiGatewayAddress}
		httpMux.Handle("/", grpcMux)
		httpApiSrv.Handler = handlers.LoggingHandler(os.Stdout, tracingWrapper(httpMux))
		go func() {
			err := httpApiSrv.ListenAndServe()
			log.WithField("err", err).Info("HTTP Gateway stopped")
		}()
		defer func() {
			err := httpApiSrv.Shutdown(ctx)
			log.Infof("Stopped HTTP API server: %v", err)
		}()

		log.Info("Serving HTTP API gateway at: ", httpApiSrv.Addr)
	}

	log.Info("Setup completed.")

	<-ctx.Done()
	log.WithField("reason", ctx.Err()).Info("Shutting down...")
	util.LogIfError(app.Close())
	time.Sleep(5 * time.Second) // Hack: wait a bit to ensure all goroutines are shutdown.
	return nil
}

func getWorkflowStore(app *App, eventPub pubsub.Publisher) func() *store.Workflows {
	var workflows *store.Workflows
	return func() *store.Workflows {
		if workflows != nil {
			return workflows
		}

		c := setupWorkflowCache(app, eventPub)
		return store.NewWorkflowsStore(c)
	}
}

func getInvocationStore(app *App, eventPub pubsub.Publisher) func() *store.Invocations {
	var invocations *store.Invocations
	return func() *store.Invocations {
		if invocations != nil {
			return invocations
		}

		c := setupWorkflowInvocationCache(app, eventPub)
		return store.NewInvocationStore(c)
	}
}

func setupInternalFunctionRuntime() *native.FunctionEnv {
	return native.NewFunctionEnv(builtin.DefaultBuiltinFunctions)
}

func setupFissionFunctionRuntime(executorAddr string, routerAddr string) *fission.FunctionEnv {
	client := executor.MakeClient(executorAddr)
	return fission.NewFunctionEnv(client, routerAddr)
}

func setupFissionFunctionResolver(controllerAddr string) *fission.Resolver {
	controllerClient := controllerc.MakeClient(controllerAddr)
	return fission.NewResolver(controllerClient)
}

func setupNatsEventStoreClient(url string, cluster string, clientID string) *nats.EventStore {
	if clientID == "" {
		clientID = util.UID()
	}

	es, err := nats.Connect(nats.Config{
		Cluster: cluster,
		URL:     url,
		Client:  clientID,
	})
	if err != nil {
		panic(err)
	}

	err = es.Watch(fes.Aggregate{Type: aggregates.TypeWorkflowInvocation})
	if err != nil {
		panic(err)
	}
	err = es.Watch(fes.Aggregate{Type: aggregates.TypeWorkflow})
	if err != nil {
		panic(err)
	}
	return es
}

func setupWorkflowInvocationCache(app *App, invocationEventPub pubsub.Publisher) *fes.SubscribedCache {
	invokeSub := invocationEventPub.Subscribe(pubsub.SubscriptionOptions{
		Buffer: 50,
		LabelMatcher: labels.Or(
			labels.In(fes.PubSubLabelAggregateType, aggregates.TypeWorkflowInvocation),
			labels.In("parent.type", aggregates.TypeWorkflowInvocation)),
	})
	name := aggregates.TypeWorkflowInvocation
	c := fes.NewSubscribedCache(fes.NewNamedMapCache(name), aggregates.NewInvocationEntity, invokeSub)
	app.RegisterCloser("cache-"+name, c)
	return c
}

func setupWorkflowCache(app *App, workflowEventPub pubsub.Publisher) *fes.SubscribedCache {
	wfSub := workflowEventPub.Subscribe(pubsub.SubscriptionOptions{
		Buffer:       10,
		LabelMatcher: labels.In(fes.PubSubLabelAggregateType, aggregates.TypeWorkflow),
	})
	name := aggregates.TypeWorkflow
	c := fes.NewSubscribedCache(fes.NewNamedMapCache(name), aggregates.NewWorkflowEntity, wfSub)
	app.RegisterCloser("cache-"+name, c)
	return c
}

func serveAdminAPI(s *grpc.Server) {
	adminServer := &apiserver.Admin{}
	apiserver.RegisterAdminAPIServer(s, adminServer)
	log.Infof("Serving admin gRPC API at %s.", gRPCAddress)
}

func serveWorkflowAPI(s *grpc.Server, es fes.Backend, resolvers map[string]fnenv.RuntimeResolver,
	store *store.Workflows) {
	workflowParser := fnenv.NewMetaResolver(resolvers)
	workflowAPI := api.NewWorkflowAPI(es, workflowParser)
	workflowServer := apiserver.NewWorkflow(workflowAPI, store)
	apiserver.RegisterWorkflowAPIServer(s, workflowServer)
	log.Infof("Serving workflow gRPC API at %s.", gRPCAddress)
}

func serveInvocationAPI(s *grpc.Server, es fes.Backend, store *store.Invocations) {
	invocationAPI := api.NewInvocationAPI(es)
	invocationServer := apiserver.NewInvocation(invocationAPI, store)
	apiserver.RegisterWorkflowInvocationAPIServer(s, invocationServer)
	log.Infof("Serving workflow invocation gRPC API at %s.", gRPCAddress)
}

func serveHTTPGateway(ctx context.Context, mux *grpcruntime.ServeMux, adminAPIAddr string, workflowAPIAddr string,
	invocationAPIAddr string) {
	tracer := opentracing.GlobalTracer()
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(grpc_opentracing.OpenTracingClientInterceptor(tracer)),
		grpc.WithStreamInterceptor(grpc_opentracing.OpenTracingStreamClientInterceptor(tracer)),
	}

	if adminAPIAddr != "" {
		err := apiserver.RegisterAdminAPIHandlerFromEndpoint(ctx, mux, adminAPIAddr, opts)
		if err != nil {
			panic(err)
		}
		log.Info("Registered Workflow API HTTP Endpoint")
	}

	if workflowAPIAddr != "" {
		err := apiserver.RegisterWorkflowAPIHandlerFromEndpoint(ctx, mux, workflowAPIAddr, opts)
		if err != nil {
			panic(err)
		}
		log.Info("Registered Admin API HTTP Endpoint")
	}

	if invocationAPIAddr != "" {
		err := apiserver.RegisterWorkflowInvocationAPIHandlerFromEndpoint(ctx, mux, invocationAPIAddr, opts)
		if err != nil {
			panic(err)
		}
		log.Info("Registered Workflow Invocation API HTTP Endpoint")
	}
}

func runFissionEnvironmentProxy(proxyMux *http.ServeMux, es fes.Backend, invocations *store.Invocations,
	workflows *store.Workflows, resolvers map[string]fnenv.RuntimeResolver) {

	workflowParser := fnenv.NewMetaResolver(resolvers)
	workflowAPI := api.NewWorkflowAPI(es, workflowParser)
	wfServer := apiserver.NewWorkflow(workflowAPI, workflows)
	wfiAPI := api.NewInvocationAPI(es)
	wfiServer := apiserver.NewInvocation(wfiAPI, invocations)
	fissionProxyServer := fission.NewFissionProxyServer(wfiServer, wfServer)
	fissionProxyServer.RegisterServer(proxyMux)
}

func setupInvocationController(invocations *store.Invocations, workflows *store.Workflows, es fes.Backend,
	fnRuntimes map[string]fnenv.Runtime, fnResolvers map[string]fnenv.RuntimeResolver) *wfictr.Controller {
	workflowAPI := api.NewWorkflowAPI(es, fnenv.NewMetaResolver(fnResolvers))
	invocationAPI := api.NewInvocationAPI(es)
	dynamicAPI := api.NewDynamicApi(workflowAPI, invocationAPI)
	taskAPI := api.NewTaskAPI(fnRuntimes, es, dynamicAPI)
	s := &scheduler.WorkflowScheduler{}
	stateStore := expr.NewStore()
	return wfictr.NewController(invocations, workflows, s, taskAPI, invocationAPI, stateStore)
}

func setupWorkflowController(store *store.Workflows, es fes.Backend,
	fnResolvers map[string]fnenv.RuntimeResolver) *wfctr.Controller {
	workflowAPI := api.NewWorkflowAPI(es, fnenv.NewMetaResolver(fnResolvers))
	return wfctr.NewController(store, workflowAPI)
}

func setupMetricsEndpoint(apiMux *http.ServeMux) {
	apiMux.Handle("/metrics", promhttp.Handler())
}

var grpcGatewayTag = opentracing.Tag{Key: string(ext.Component), Value: "grpc-gateway"}

func tracingWrapper(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parentSpanContext, err := opentracing.GlobalTracer().Extract(
			opentracing.HTTPHeaders,
			opentracing.HTTPHeadersCarrier(r.Header))
		if err == nil || err == opentracing.ErrSpanContextNotFound {
			serverSpan := opentracing.GlobalTracer().StartSpan(
				"ServeHTTP",
				// this is magical, it attaches the new span to the parent parentSpanContext,
				// and creates an unparented one if empty.
				ext.RPCServerOption(parentSpanContext),
				grpcGatewayTag,
				opentracing.Tag{Key: string(ext.HTTPMethod), Value: r.Method},
				opentracing.Tag{Key: string(ext.HTTPUrl), Value: r.URL},
			)
			r = r.WithContext(opentracing.ContextWithSpan(r.Context(), serverSpan))
			defer serverSpan.Finish()
		} else {
			log.Errorf("Failed to extract tracer from HTTP request: %v", err)
		}
		h.ServeHTTP(w, r)
	})
}
