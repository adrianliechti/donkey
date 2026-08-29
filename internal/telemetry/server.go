package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	codepb "google.golang.org/genproto/googleapis/rpc/code"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const maxIngestBody = 8 << 20

type Config struct {
	OTLPListen    string
	APIListen     string
	DatabasePath  string
	RetentionDays int
	Key           []byte
}

func ConfigFromEnv() (*Config, error) {
	retention := DefaultRetentionDays
	if raw := os.Getenv("DONKEY_TELEMETRY_RETENTION_DAYS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > DefaultRetentionDays {
			return nil, fmt.Errorf("DONKEY_TELEMETRY_RETENTION_DAYS must be between 1 and %d", DefaultRetentionDays)
		}
		retention = value
	}
	key, err := hex.DecodeString(os.Getenv("DONKEY_TELEMETRY_KEY"))
	if err != nil || len(key) != 32 {
		return nil, errors.New("DONKEY_TELEMETRY_KEY must be 32 bytes of hex")
	}
	return &Config{
		OTLPListen:    envDefault("DONKEY_TELEMETRY_LISTEN", ":4318"),
		APIListen:     envDefault("DONKEY_TELEMETRY_API_LISTEN", ":9090"),
		DatabasePath:  envDefault("DONKEY_TELEMETRY_DB", "/var/lib/donkey/telemetry.duckdb"),
		RetentionDays: retention,
		Key:           key,
	}, nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type Service struct {
	cfg   *Config
	store *Store
}

func NewService(cfg *Config) (*Service, error) {
	if cfg == nil || len(cfg.Key) != 32 {
		return nil, errors.New("telemetry requires a 32-byte installation key")
	}
	store, err := OpenStore(cfg.DatabasePath, cfg.RetentionDays)
	if err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, store: store}, nil
}

func Run(ctx context.Context, cfg *Config) error {
	service, err := NewService(cfg)
	if err != nil {
		return err
	}
	defer service.store.Close()

	otlp := &http.Server{Addr: cfg.OTLPListen, Handler: service.ingestHandler(), ReadHeaderTimeout: 10 * time.Second}
	api := &http.Server{Addr: cfg.APIListen, Handler: service.apiHandler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	serve := func(name string, server *http.Server) {
		log.Printf("donkey-telemetry %s listening on %s", name, server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}
	go serve("OTLP/HTTP", otlp)
	go serve("query API", api)
	go service.cleanupLoop(ctx)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err = otlp.Shutdown(shutdown)
	if apiErr := api.Shutdown(shutdown); err == nil {
		err = apiErr
	}
	return err
}

func (s *Service) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.store.Cleanup(ctx, now); err != nil && ctx.Err() == nil {
				log.Printf("telemetry retention cleanup: %v", err)
			}
		}
	}
}

func (s *Service) ingestHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("POST /v1/metrics", s.handleMetrics)
	mux.HandleFunc("POST /v1/donkey/views", s.handleViews)
	return mux
}

func (s *Service) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("GET /v1/traffic", s.requireQueryToken(s.handleTraffic))
	mux.HandleFunc("GET /v1/resources", s.requireQueryToken(s.handleResources))
	mux.HandleFunc("PUT /v1/builds", s.requireQueryToken(s.handlePutBuild))
	mux.HandleFunc("GET /v1/builds", s.requireQueryToken(s.handleBuilds))
	mux.HandleFunc("GET /v1/builds/record", s.requireQueryToken(s.handleBuildRecord))
	mux.HandleFunc("DELETE /v1/spaces/{space}", s.requireQueryToken(s.handleDeleteSpace))
	return mux
}

// buildEnvelope is the wire shape of one stored build. The record itself is
// the server's JSON, carried opaquely: this service versions and lists build
// history, it does not interpret it.
type buildEnvelope struct {
	Space    string          `json:"space"`
	App      string          `json:"app"`
	Image    string          `json:"image,omitempty"`
	OK       bool            `json:"ok"`
	Sequence int64           `json:"sequence,omitempty"`
	Record   json.RawMessage `json:"record,omitempty"`
}

func (s *Service) handlePutBuild(w http.ResponseWriter, r *http.Request) {
	var envelope buildEnvelope
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody))
	if err := decoder.Decode(&envelope); err != nil {
		http.Error(w, "invalid build record payload", http.StatusBadRequest)
		return
	}
	sequence, err := s.store.InsertBuild(r.Context(),
		envelope.Space, envelope.App, envelope.Image, envelope.OK, string(envelope.Record), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeServiceJSON(w, http.StatusOK, buildEnvelope{
		Space: envelope.Space, App: envelope.App, Sequence: sequence,
	})
}

func (s *Service) handleBuilds(w http.ResponseWriter, r *http.Request) {
	builds, err := s.store.Builds(r.Context(), r.URL.Query().Get("space"), r.URL.Query().Get("app"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeServiceJSON(w, http.StatusOK, map[string]any{"builds": builds})
}

func (s *Service) handleBuildRecord(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	space, app := query.Get("space"), query.Get("app")
	var record string
	var err error
	if image := query.Get("image"); image != "" {
		record, err = s.store.BuildByImage(r.Context(), space, app, image)
	} else {
		var sequence int64
		if sequence, err = strconv.ParseInt(query.Get("sequence"), 10, 64); err != nil {
			http.Error(w, "sequence or image is required", http.StatusBadRequest)
			return
		}
		record, err = s.store.Build(r.Context(), space, app, sequence)
	}
	if errors.Is(err, ErrNoBuild) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, record)
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ready(r.Context()); err != nil {
		writeServiceJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeServiceJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) handleMetrics(w http.ResponseWriter, r *http.Request) {
	req := new(collectormetricspb.ExportMetricsServiceRequest)
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = "application/x-protobuf"
	}
	body, err := ingestBody(w, r)
	if err != nil {
		writeOTLPError(w, mediaType, http.StatusBadRequest, codepb.Code_INVALID_ARGUMENT, err.Error())
		return
	}
	switch mediaType {
	case "application/x-protobuf", "application/protobuf":
		err = proto.Unmarshal(body, req)
	case "application/json":
		err = protojson.Unmarshal(body, req)
	default:
		writeOTLPError(w, mediaType, http.StatusUnsupportedMediaType, codepb.Code_INVALID_ARGUMENT,
			"OTLP/HTTP metrics require protobuf or JSON")
		return
	}
	if err != nil {
		writeOTLPError(w, mediaType, http.StatusBadRequest, codepb.Code_INVALID_ARGUMENT, "invalid OTLP metrics payload")
		return
	}
	principal, ok := metricPrincipal(req, r.Header.Get(TelemetryTokenHeader), s.cfg.Key)
	if !ok {
		writeOTLPError(w, mediaType, http.StatusUnauthorized, codepb.Code_UNAUTHENTICATED, "invalid telemetry scope token")
		return
	}
	var rejected int64
	var partialMessage string
	switch principal {
	case metricPrincipalRuntime:
		_, rejected, err = s.store.IngestResourceMetrics(r.Context(), req)
		partialMessage = "only valid OpenTelemetry container CPU usage and memory working-set metrics are retained"
	case metricPrincipalGateway, metricPrincipalApp:
		_, rejected, err = s.store.IngestMetrics(r.Context(), req, principal == metricPrincipalGateway)
		partialMessage = "only valid OpenTelemetry HTTP server duration histograms for the authenticated app scope are retained"
	default:
		writeOTLPError(w, mediaType, http.StatusUnauthorized, codepb.Code_UNAUTHENTICATED, "invalid telemetry scope token")
		return
	}
	if err != nil {
		// 503 is one of OTLP's retryable codes; a 500 would make conformant
		// exporters drop the batch a transient storage hiccup only delayed.
		w.Header().Set("Retry-After", "5")
		writeOTLPError(w, mediaType, http.StatusServiceUnavailable, codepb.Code_UNAVAILABLE, "storing metrics failed")
		return
	}
	response := &collectormetricspb.ExportMetricsServiceResponse{}
	if rejected > 0 {
		response.PartialSuccess = &collectormetricspb.ExportMetricsPartialSuccess{
			RejectedDataPoints: rejected,
			ErrorMessage:       partialMessage,
		}
	}
	writeOTLPResponse(w, mediaType, response)
}

func ingestBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, maxIngestBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, errors.New("invalid gzip body")
		}
		defer gz.Close()
		reader = gz
	} else if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxIngestBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxIngestBody {
		return nil, errors.New("telemetry payload is too large")
	}
	return body, nil
}

// otlpContentType mirrors the request's media type, as OTLP/HTTP requires —
// including the lenient application/protobuf alias — and answers anything
// unrecognized in the protocol's default encoding.
func otlpContentType(mediaType string) string {
	switch mediaType {
	case "application/json", "application/protobuf":
		return mediaType
	}
	return "application/x-protobuf"
}

func writeOTLPMessage(w http.ResponseWriter, mediaType string, httpStatus int, message proto.Message) {
	contentType := otlpContentType(mediaType)
	var body []byte
	var err error
	if contentType == "application/json" {
		body, err = protojson.Marshal(message)
	} else {
		body, err = proto.Marshal(message)
	}
	if err != nil {
		http.Error(w, "encoding OTLP response failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(httpStatus)
	_, _ = w.Write(body)
}

func writeOTLPResponse(w http.ResponseWriter, mediaType string, response proto.Message) {
	writeOTLPMessage(w, mediaType, http.StatusOK, response)
}

// writeOTLPError answers a failed export. OTLP/HTTP requires every 4xx/5xx
// body to be a google.rpc.Status in the request's encoding — a text/plain
// body is a protocol violation clients cannot parse.
func writeOTLPError(w http.ResponseWriter, mediaType string, httpStatus int, code codepb.Code, message string) {
	writeOTLPMessage(w, mediaType, httpStatus, &statuspb.Status{Code: int32(code), Message: message})
}

func (s *Service) handleViews(w http.ResponseWriter, r *http.Request) {
	if !tokenMatches(r.Header.Get(TelemetryTokenHeader), GatewayToken(s.cfg.Key)) {
		http.Error(w, "gateway token required", http.StatusUnauthorized)
		return
	}
	var batch ViewBatch
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil {
		http.Error(w, "invalid view batch", http.StatusBadRequest)
		return
	}
	accepted, rejected, err := s.store.InsertViews(r.Context(), batch, time.Now())
	if err != nil {
		http.Error(w, "storing views failed", http.StatusInternalServerError)
		return
	}
	writeServiceJSON(w, http.StatusOK, map[string]int{"accepted": accepted, "rejected": rejected})
}

func (s *Service) requireQueryToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !tokenMatches(token, QueryToken(s.cfg.Key)) {
			http.Error(w, "query token required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Service) handleTraffic(w http.ResponseWriter, r *http.Request) {
	selected, err := ParseTrafficRange(r.URL.Query().Get("range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report, err := s.store.Traffic(r.Context(), r.URL.Query().Get("space"), r.URL.Query().Get("app"), r.URL.Query().Get("source"), selected, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeServiceJSON(w, http.StatusOK, report)
}

func (s *Service) handleResources(w http.ResponseWriter, r *http.Request) {
	selected, err := ParseResourceRange(r.URL.Query().Get("range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report, err := s.store.Resources(
		r.Context(), r.URL.Query().Get("space"), r.URL.Query().Get("app"), selected, time.Now(),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeServiceJSON(w, http.StatusOK, report)
}

func (s *Service) handleDeleteSpace(w http.ResponseWriter, r *http.Request) {
	if err := s.store.PurgeSpace(r.Context(), r.PathValue("space")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeServiceJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeServiceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
