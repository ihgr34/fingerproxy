package fingerproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wi1dcard/fingerproxy/pkg/debug"
	"github.com/wi1dcard/fingerproxy/pkg/fingerprint"
	"github.com/wi1dcard/fingerproxy/pkg/proxyserver"
	"github.com/wi1dcard/fingerproxy/pkg/reverseproxy"
)

const logFlags = log.LstdFlags | log.Lshortfile | log.Lmsgprefix

var (
	// values are from CI build
	BuildCommit = "GIT_COMMIT_PLACEHOLDER"
	BuildTag    = "GIT_TAG_PLACEHOLDER"
)

var (
	// The loggers used by fingerproxy components

	ProxyServerLog  = log.New(os.Stderr, "[proxyserver] ", logFlags)
	HTTPServerLog   = log.New(os.Stderr, "[http] ", logFlags)
	PrometheusLog   = log.New(os.Stderr, "[metrics] ", logFlags)
	ReverseProxyLog = log.New(os.Stderr, "[reverseproxy] ", logFlags)
	FingerprintLog  = log.New(os.Stderr, "[fingerprint] ", logFlags)
	DefaultLog      = log.New(os.Stderr, "[fingerproxy] ", logFlags)

	// The Prometheus metric registry used by fingerproxy
	PrometheusRegistry = prometheus.NewRegistry()

	// The header injectors that injects fingerprint headers to forwarding requests,
	// defaults to [fingerproxy.DefaultHeaderInjectors]
	GetHeaderInjectors = DefaultHeaderInjectors
)

// DefaultHeaderInjectors is the default header injector set that injects JA3, JA4,
// and Akamai HTTP2 fingerprints. Override [fingerproxy.GetHeaderInjectors] to replace
// this to your own injectors.
func DefaultHeaderInjectors() []reverseproxy.HeaderInjector {
	return []reverseproxy.HeaderInjector{
		fingerprint.NewFingerprintHeaderInjector("X-JA3-Fingerprint", fingerprint.JA3Fingerprint),
		fingerprint.NewFingerprintHeaderInjector("X-JA4-Fingerprint", fingerprint.JA4Fingerprint),
		fingerprint.NewFingerprintHeaderInjector("X-HTTP2-Fingerprint", fingerprint.HTTP2Fingerprint),
	}
}

func proxyErrorHandler(rw http.ResponseWriter, req *http.Request, err error) {
	ReverseProxyLog.Printf("proxy %s error (from %s): %v", req.URL.String(), req.RemoteAddr, err)

	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		rw.WriteHeader(http.StatusGatewayTimeout)
	} else {
		rw.WriteHeader(http.StatusBadGateway)
	}
}

func defaultReverseProxyHTTPHandler(forwardTo *url.URL, headerInjectors []reverseproxy.HeaderInjector) http.Handler {
	handler := reverseproxy.NewHTTPHandler(
		forwardTo,
		&httputil.ReverseProxy{
			ErrorLog:      ReverseProxyLog,
			FlushInterval: parseReverseProxyFlushInterval(),
			ErrorHandler:  proxyErrorHandler,
			// TODO: customize transport
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		},
		headerInjectors,
	)

	handler.PreserveHost = *flagPreserveHost

	if *flagEnableKubernetesProbe {
		handler.IsProbeRequest = reverseproxy.IsKubernetesProbeRequest
	}

	return handler
}

func defaultProxyServer(handler http.Handler, tlsConfig *tls.Config) *proxyserver.Server {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	svr := proxyserver.NewServer(ctx, handler, tlsConfig)

	svr.VerboseLogs = *flagVerboseLogs
	svr.ErrorLog = ProxyServerLog
	svr.HTTPServer.ErrorLog = HTTPServerLog

	svr.MetricsRegistry = PrometheusRegistry

	svr.HTTPServer.IdleTimeout = parseHTTPIdleTimeout()
	svr.HTTPServer.ReadTimeout = parseHTTPReadTimeout()
	svr.HTTPServer.WriteTimeout = parseHTTPWriteTimeout()
	svr.TLSHandshakeTimeout = parseTLSHandshakeTimeout()

	return svr
}

func initFingerprint() {
	fingerprint.Logger = FingerprintLog
	fingerprint.VerboseLogs = *flagVerboseLogs
	fingerprint.RegisterDurationMetric(PrometheusRegistry, parseDurationMetricBuckets(), "")
}

// Run fingerproxy. To customize the fingerprinting algorithms, use "header injectors".
// See [fingerproxy.GetHeaderInjectors] for more info.
func Run() {
	// CLI
	initFlags()
	parseFlags()

	// fingerprint package
	initFingerprint()

	// main TLS server
	server := defaultProxyServer(
		defaultReverseProxyHTTPHandler(
			parseForwardURL(),
			GetHeaderInjectors(),
		),
		&tls.Config{
			NextProtos:   []string{"h2", "http/1.1"},
			MinVersion:   tls.VersionTLS12,
			MaxVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{parseTLSCerts()},
		},
	)

	// metrics server
	PrometheusLog.Printf("server listening on %s", *flagMetricsListenAddr)
	go http.ListenAndServe(
		*flagMetricsListenAddr,
		promhttp.HandlerFor(PrometheusRegistry, promhttp.HandlerOpts{
			ErrorLog: PrometheusLog,
		}),
	)

	// debug server if binary build with `debug` tag
	debug.StartDebugServer()

	// start the main TLS server
	DefaultLog.Printf("server listening on %s", *flagListenAddr)
	err := server.ListenAndServe(*flagListenAddr)
	DefaultLog.Print(err)
}
