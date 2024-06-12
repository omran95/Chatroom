package infrastructure

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

	"github.com/go-kit/kit/circuitbreaker"
	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/sd"
	"github.com/go-kit/kit/sd/lb"
	grpctransport "github.com/go-kit/kit/transport/grpc"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"github.com/omran95/chat-app/pkg/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

var (
	ServiceIdHeader string = "Service-Id"
)

func interceptorLogger(l common.GrpcLog) logging.Logger {
	return logging.LoggerFunc(func(_ context.Context, lvl logging.Level, msg string, fields ...any) {
		switch lvl {
		case logging.LevelDebug:
			l.Debug(msg, fields...)
		case logging.LevelInfo:
			l.Info(msg, fields...)
		case logging.LevelWarn:
			l.Warn(msg, fields...)
		case logging.LevelError:
			l.Error(msg, fields...)
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}
	})
}

func InitializeGrpcServer(name string, logger common.GrpcLog) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(1024 * 1024 * 8), // increase to 8 MB (default: 4 MB)
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // terminate the connection if a client pings more than once every 5 seconds
			PermitWithoutStream: true,            // allow pings even when there are no active streams
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     15 * time.Second,
			MaxConnectionAge:      600 * time.Second,
			MaxConnectionAgeGrace: 5 * time.Second,
			Time:                  5 * time.Second,
			Timeout:               1 * time.Second,
		}),
	}

	srvMetrics := grpcprom.NewServerMetrics(
		grpcprom.WithServerCounterOptions(
			func(o *prometheus.CounterOpts) {
				o.Namespace = name
			},
			grpcprom.WithConstLabels(prometheus.Labels{"serviceID": name}),
		),
		grpcprom.WithServerHandlingTimeHistogram(
			grpcprom.WithHistogramConstLabels(prometheus.Labels{"serviceID": name}),
			grpcprom.WithHistogramBuckets([]float64{0.001, 0.01, 0.1, 0.3, 0.6, 1, 3, 6, 9, 20, 30, 60, 90, 120}),
		),
	)
	prometheus.MustRegister(srvMetrics)
	exemplarFromContext := func(ctx context.Context) prometheus.Labels {
		if span := trace.SpanContextFromContext(ctx); span.IsSampled() {
			return prometheus.Labels{"traceID": span.TraceID().String()}
		}
		return nil
	}

	panicsTotal := promauto.NewCounter(prometheus.CounterOpts{
		Namespace:   name,
		Name:        "grpc_req_panics_recovered_total",
		Help:        "Total number of gRPC requests recovered from internal panic.",
		ConstLabels: prometheus.Labels{"serviceID": name},
	})
	grpcPanicRecoveryHandler := func(p any) (err error) {
		panicsTotal.Inc()
		logger.Error("recovered from panic, stack: " + string(debug.Stack()))
		return status.Errorf(codes.Internal, "%s", p)
	}
	logTraceID := func(ctx context.Context) logging.Fields {
		if span := trace.SpanContextFromContext(ctx); span.IsSampled() {
			return logging.Fields{"traceID", span.TraceID().String()}
		}
		return nil
	}
	logOpts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
		logging.WithDurationField(logging.DurationToTimeMillisFields),
		logging.WithFieldsFromContext(logTraceID),
	}

	opts = append(opts,
		grpc.ChainStreamInterceptor(
			otelgrpc.StreamServerInterceptor(),
			srvMetrics.StreamServerInterceptor(grpcprom.WithExemplarFromContext(exemplarFromContext)),
			logging.StreamServerInterceptor(interceptorLogger(logger), logOpts...),
			recovery.StreamServerInterceptor(recovery.WithRecoveryHandler(grpcPanicRecoveryHandler)),
		),
		grpc.ChainUnaryInterceptor(
			otelgrpc.UnaryServerInterceptor(),
			srvMetrics.UnaryServerInterceptor(grpcprom.WithExemplarFromContext(exemplarFromContext)),
			logging.UnaryServerInterceptor(interceptorLogger(logger), logging.WithFieldsFromContext(logTraceID)),
			recovery.UnaryServerInterceptor(recovery.WithRecoveryHandler(grpcPanicRecoveryHandler)),
		),
	)
	grpcSrv := grpc.NewServer(opts...)
	srvMetrics.InitializeMetrics(grpcSrv)
	return grpcSrv
}

func InitializeGrpcClient(svcHost string) (*grpc.ClientConn, error) {
	scheme := "dns"

	retryOpts := []retry.CallOption{
		// generate waits between 900ms to 1100ms
		retry.WithBackoff(retry.BackoffLinearWithJitter(1*time.Second, 0.1)),
		retry.WithMax(3),
		retry.WithCodes(codes.Unavailable, codes.Aborted),
		retry.WithPerRetryTimeout(3 * time.Second),
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	dialOpts = append(dialOpts,
		grpc.WithDisableServiceConfig(),
		grpc.WithDefaultServiceConfig(`{
			"loadBalancingPolicy": "round_robin"
		}`),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithUnaryInterceptor(retry.UnaryClientInterceptor(retryOpts...)),
		grpc.WithStreamInterceptor(retry.StreamClientInterceptor(retryOpts...)),
	)

	slog.Info("connecting to grpc host: " + svcHost)
	conn, err := grpc.NewClient(
		fmt.Sprintf("%s:///%s", scheme, svcHost),
		dialOpts...,
	)
	if err != nil {
		slog.Error("error in connecting to grpc host: " + err.Error())
		return nil, err
	}
	return conn, nil
}

func NewGrpcEndpoint(conn *grpc.ClientConn, serviceID, serviceName, method string, grpcReply interface{}) endpoint.Endpoint {
	var options []grpctransport.ClientOption
	var (
		ep         endpoint.Endpoint
		endpointer sd.FixedEndpointer
	)

	ep = grpctransport.NewClient(
		conn,
		serviceName,
		method,
		encodeGRPCRequest,
		decodeGRPCResponse,
		grpcReply,
		append(options, grpctransport.ClientBefore(grpctransport.SetRequestHeader(ServiceIdHeader, serviceID)))...,
	).Endpoint()
	ep = circuitbreaker.Gobreaker(gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:    serviceName + "." + method,
		Timeout: 60 * time.Second,
	}))(ep)
	endpointer = append(endpointer, ep)
	ep = lb.Retry(1, 15*time.Second, lb.NewRoundRobin(endpointer))

	return ep
}

func encodeGRPCRequest(_ context.Context, request interface{}) (interface{}, error) {
	return request, nil
}

func decodeGRPCResponse(_ context.Context, grpcReply interface{}) (interface{}, error) {
	return grpcReply, nil
}
