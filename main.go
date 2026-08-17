// Command ufm-mock serves the NVIDIA UFM Enterprise REST contract against an
// in-memory InfiniBand fabric, so UFM clients can be developed and demoed
// without a real subnet manager.
//
// The contract mirrors NVIDIA's own Rust mock in
// github.com/NVIDIA/infra-controller, crates/ufm-mock (Apache-2.0).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// apiPrefixes are the three UFM auth flavours. They share one router, exactly
// as a real UFM does: /ufmRest (basic), /ufmRestV2 (client cert),
// /ufmRestV3 (token).
var apiPrefixes = []string{"/ufmRestV3", "/ufmRestV2", "/ufmRest"}

type config struct {
	listen   string
	token    string
	version  string
	seedFile string
	user     string
	password string
	ports    int
}

func loadConfig() config {
	return config{
		listen:   env("UFM_MOCK_LISTEN", "0.0.0.0:9888"),
		token:    env("UFM_MOCK_AUTH_TOKEN", "demo-token"),
		version:  env("UFM_MOCK_VERSION", "6.18.0"),
		seedFile: env("UFM_MOCK_SEED", ""),
		user:     env("UFM_MOCK_USER", "admin"),
		password: env("UFM_MOCK_PASSWORD", "123456"),
		ports:    envInt("UFM_MOCK_PORTS", 8),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
// fault injection
// ---------------------------------------------------------------------------

// InjectionRule forces a status code on requests matching a method and path
// glob, so failure handling can be demonstrated on demand.
type InjectionRule struct {
	ID       string `json:"id"`
	Selector struct {
		Path struct {
			Method string `json:"method"`
			Glob   string `json:"glob"`
		} `json:"Path"`
	} `json:"selector"`
	Action struct {
		Status int `json:"Status"`
	} `json:"action"`
}

type injectionStore struct {
	mu    sync.RWMutex
	rules []InjectionRule
}

func (s *injectionStore) add(rule InjectionRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, rule)
}

func (s *injectionStore) replace(rules []InjectionRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = rules
}

func (s *injectionStore) list() []InjectionRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]InjectionRule, len(s.rules))
	copy(out, s.rules)
	return out
}

// match reports the status to force, or 0 when no rule applies.
func (s *injectionStore) match(method, requestPath string) (int, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rule := range s.rules {
		selector := rule.Selector.Path
		if selector.Method != "" && !strings.EqualFold(selector.Method, method) {
			continue
		}
		glob := selector.Glob
		if glob == "" {
			glob = "*"
		}
		if ok, err := path.Match(glob, requestPath); err != nil || !ok {
			continue
		}
		if rule.Action.Status != 0 {
			return rule.Action.Status, rule.ID
		}
	}
	return 0, ""
}

// ---------------------------------------------------------------------------
// server
// ---------------------------------------------------------------------------

type server struct {
	config    config
	fabric    *Fabric
	events    *EventLog
	syslog    *SyslogSender
	injection *injectionStore
	basicAuth string // pre-encoded "Basic <base64(user:pass)>"

	requests     atomic.Int64
	unauthorized atomic.Int64
	started      time.Time
}

func newServer(cfg config) *server {
	credentials := base64.StdEncoding.EncodeToString([]byte(cfg.user + ":" + cfg.password))
	srv := &server{
		config:    cfg,
		fabric:    NewFabric(cfg.version),
		events:    NewEventLog(1000),
		syslog:    NewSyslogSender("ufm-mock"),
		injection: &injectionStore{},
		basicAuth: "Basic " + credentials,
		started:   time.Now(),
	}

	// Every event that passes the syslog filter goes out as a real datagram,
	// which is what the EFS forwarder consumes.
	srv.events.SetEmitter(func(event Event) {
		destination := srv.events.SyslogConfig().Destination
		if err := srv.syslog.Send(destination, event); err != nil {
			log.Printf("syslog emit failed: %v", err)
		}
	})
	return srv
}

func (s *server) writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to serialize UFM response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("x-ufm-mock", "carbide-ufm-mock")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *server) writeError(w http.ResponseWriter, err error) {
	var fabricErr *FabricError
	if errors.As(err, &fabricErr) {
		s.writeJSON(w, fabricErr.Status, map[string]string{"error": fabricErr.Message})
		return
	}
	s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func decodeBody[T any](r *http.Request) (T, error) {
	var payload T
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return payload, fmt.Errorf("invalid request body: %w", err)
	}
	return payload, nil
}

// authorized accepts the raw V3 token, a Bearer token, or real basic auth so
// all three UFM prefixes can be demonstrated.
func (s *server) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	switch {
	case header == "Basic "+s.config.token:
		return true
	case header == s.basicAuth:
		return true
	case strings.HasPrefix(header, "Bearer "):
		return strings.TrimPrefix(header, "Bearer ") == s.config.token
	default:
		return false
	}
}

func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

func (s *server) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		requestPath := r.URL.Path
		if len(requestPath) > 1 {
			requestPath = strings.TrimRight(requestPath, "/")
		}

		// Unauthenticated management surface.
		switch requestPath {
		case "/metrics":
			s.handleMetrics(w)
			return
		case "/health", "/healthz":
			s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		case "/ui", "/":
			s.handleUI(w, r)
			return
		case "/Injection/rules":
			s.handleInjection(w, r)
			return
		case "/admin/inventory":
			s.handleInventory(w, r)
			return
		}

		prefix := ""
		for _, candidate := range apiPrefixes {
			if strings.HasPrefix(requestPath, candidate+"/") {
				prefix = candidate
				break
			}
		}
		if prefix == "" {
			s.writeJSON(w, http.StatusNotFound,
				map[string]string{"error": "no route for " + requestPath})
			return
		}

		if !s.authorized(r) {
			s.unauthorized.Add(1)
			s.writeJSON(w, http.StatusUnauthorized,
				map[string]string{"error": "invalid UFM authorization token"})
			return
		}

		if status, id := s.injection.match(r.Method, requestPath); status != 0 {
			s.writeJSON(w, status, map[string]string{"error": "injected by rule " + id})
			return
		}

		s.route(w, r, strings.TrimPrefix(requestPath, prefix))
	})
}

func (s *server) route(w http.ResponseWriter, r *http.Request, route string) {
	switch {
	case r.Method == http.MethodGet && route == "/app/ufm_version":
		s.writeJSON(w, http.StatusOK,
			map[string]string{"ufm_release_version": s.fabric.Version})

	case r.Method == http.MethodGet && route == "/app/smconf":
		s.writeJSON(w, http.StatusOK, s.fabric.SMConfig)

	// ---- events ----
	case r.Method == http.MethodGet && route == "/app/events":
		query := r.URL.Query()
		s.writeJSON(w, http.StatusOK, s.events.Events(EventFilter{
			ObjectName: query.Get("object_name"),
			Type:       query.Get("type"),
			Category:   query.Get("category"),
			Severity:   query.Get("severity"),
			Group:      query.Get("group"),
		}))

	case r.Method == http.MethodPost && route == "/app/events/external_event":
		request, err := decodeBody[ExternalEventRequest](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, s.events.Raise(request))

	case r.Method == http.MethodPost && route == "/app/events/external_events":
		requests, err := decodeBody[[]ExternalEventRequest](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		raised := make([]Event, 0, len(requests))
		for _, request := range requests {
			raised = append(raised, s.events.Raise(request))
		}
		s.writeJSON(w, http.StatusOK, raised)

	case r.Method == http.MethodGet && strings.HasPrefix(route, "/app/events/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(route, "/app/events/"), 10, 64)
		if err != nil {
			s.writeError(w, badRequest("invalid event id"))
			return
		}
		event, ok := s.events.Event(id)
		if !ok {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
			return
		}
		s.writeJSON(w, http.StatusOK, event)

	// ---- alarms ----
	case r.Method == http.MethodGet && route == "/app/alarms":
		s.writeJSON(w, http.StatusOK, s.events.Alarms(r.URL.Query().Get("device_id")))

	case r.Method == http.MethodDelete && route == "/app/alarms":
		// device_id is optional here; omitting it clears the whole board,
		// which is what you want between demo runs.
		device := r.URL.Query().Get("device_id")
		removed := s.events.DeleteAlarms(device)
		s.writeJSON(w, http.StatusOK, map[string]any{"deleted": removed, "device_id": device})

	case r.Method == http.MethodGet && strings.HasPrefix(route, "/app/alarms/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(route, "/app/alarms/"), 10, 64)
		if err != nil {
			s.writeError(w, badRequest("invalid alarm id"))
			return
		}
		alarm, ok := s.events.Alarm(id)
		if !ok {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "alarm not found"})
			return
		}
		s.writeJSON(w, http.StatusOK, alarm)

	case r.Method == http.MethodDelete && strings.HasPrefix(route, "/app/alarms/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(route, "/app/alarms/"), 10, 64)
		if err != nil {
			s.writeError(w, badRequest("invalid alarm id"))
			return
		}
		if !s.events.DeleteAlarm(id) {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "alarm not found"})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"deleted": 1})

	// ---- syslog configuration, which drives the EFS pipeline ----
	case r.Method == http.MethodGet && route == "/app/syslog":
		s.writeJSON(w, http.StatusOK, s.events.SyslogConfig())

	case (r.Method == http.MethodPut || r.Method == http.MethodPost) && route == "/app/syslog":
		request, err := decodeBody[SyslogConfig](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		applied := s.events.SetSyslogConfig(request)
		log.Printf("syslog config: active=%v destination=%s level=%s",
			applied.Active, applied.Destination, applied.Level)
		s.writeJSON(w, http.StatusOK, applied)

	case r.Method == http.MethodGet && route == "/resources/ports":
		s.writeJSON(w, http.StatusOK, s.fabric.Ports())

	case r.Method == http.MethodGet && route == "/resources/systems":
		// Convenience view used by UFM browsers, derived from the port list.
		systems := make([]map[string]string, 0)
		for _, port := range s.fabric.Ports() {
			systems = append(systems, map[string]string{
				"guid":        port.SystemID,
				"name":        port.SystemName,
				"system_type": "Computer",
				"severity":    "Info",
			})
		}
		s.writeJSON(w, http.StatusOK, systems)

	case r.Method == http.MethodGet && route == "/resources/pkeys":
		s.writeJSON(w, http.StatusOK,
			s.fabric.Partitions(boolParam(r, "guids_data"), boolParam(r, "qos_conf")))

	case r.Method == http.MethodPost && route == "/resources/pkeys":
		request, err := decodeBody[BindRequest](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if err := s.fabric.Bind(request); err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{})

	case r.Method == http.MethodPut && route == "/resources/pkeys/qos_conf":
		request, err := decodeBody[QoSRequest](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if err := s.fabric.UpdateQoS(request); err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{})

	case r.Method == http.MethodPost && route == "/actions/remove_guids_from_pkey":
		request, err := decodeBody[UnbindRequest](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if err := s.fabric.Unbind(request); err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{})

	case r.Method == http.MethodGet && strings.HasPrefix(route, "/resources/pkeys/"):
		key, err := ParsePKey(strings.TrimPrefix(route, "/resources/pkeys/"))
		if err != nil {
			s.writeError(w, err)
			return
		}
		data, ok := s.fabric.Partition(key, boolParam(r, "guids_data"), boolParam(r, "qos_conf"))
		if !ok {
			// UFM answers 200 with an empty object for an unknown pkey.
			s.writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		s.writeJSON(w, http.StatusOK, data)

	default:
		s.writeJSON(w, http.StatusNotFound,
			map[string]string{"error": "no route for " + r.Method + " " + route})
	}
}

func (s *server) handleInjection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, s.injection.list())
	case http.MethodPost:
		rule, err := decodeBody[InjectionRule](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.injection.add(rule)
		s.writeJSON(w, http.StatusOK, map[string]any{})
	case http.MethodPut:
		rules, err := decodeBody[[]InjectionRule](r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.injection.replace(rules)
		s.writeJSON(w, http.StatusOK, map[string]any{})
	case http.MethodDelete:
		s.injection.replace(nil)
		s.writeJSON(w, http.StatusOK, map[string]any{})
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleInventory ingests a complete inventory snapshot, standing in for the
// machine-a-tron sources the real mock polls.
func (s *server) handleInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	snapshot, err := decodeBody[InventorySnapshot](r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	outcome, err := s.fabric.Reconcile(snapshot)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ports, _, _, _ := s.fabric.Stats()
	s.writeJSON(w, http.StatusOK, map[string]any{"outcome": outcome, "ports": ports})
}

func (s *server) handleMetrics(w http.ResponseWriter) {
	ports, active, partitions, sources := s.fabric.Stats()
	var builder strings.Builder
	fmt.Fprintf(&builder, "# HELP ufm_mock_ports Ports in the simulated fabric.\n")
	fmt.Fprintf(&builder, "# TYPE ufm_mock_ports gauge\n")
	fmt.Fprintf(&builder, "ufm_mock_ports %d\n", ports)
	fmt.Fprintf(&builder, "ufm_mock_ports_active %d\n", active)
	fmt.Fprintf(&builder, "# HELP ufm_mock_partitions Partitions (pkeys) in the fabric.\n")
	fmt.Fprintf(&builder, "# TYPE ufm_mock_partitions gauge\n")
	fmt.Fprintf(&builder, "ufm_mock_partitions %d\n", partitions)
	fmt.Fprintf(&builder, "ufm_mock_inventory_sources %d\n", sources)
	eventCount, alarmCount, criticalCount := s.events.Counts()
	fmt.Fprintf(&builder, "# HELP ufm_mock_alarms Active alarms.\n")
	fmt.Fprintf(&builder, "# TYPE ufm_mock_alarms gauge\n")
	fmt.Fprintf(&builder, "ufm_mock_alarms %d\n", alarmCount)
	fmt.Fprintf(&builder, "ufm_mock_alarms_critical %d\n", criticalCount)
	fmt.Fprintf(&builder, "ufm_mock_events_total %d\n", eventCount)
	fmt.Fprintf(&builder, "# HELP ufm_mock_requests_total Requests served.\n")
	fmt.Fprintf(&builder, "# TYPE ufm_mock_requests_total counter\n")
	fmt.Fprintf(&builder, "ufm_mock_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(&builder, "ufm_mock_unauthorized_total %d\n", s.unauthorized.Load())
	fmt.Fprintf(&builder, "ufm_mock_uptime_seconds %d\n", int(time.Since(s.started).Seconds()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, builder.String())
}

// seed loads an initial topology from UFM_MOCK_SEED, or synthesises one.
func (s *server) seed() error {
	if s.config.seedFile != "" {
		raw, err := os.ReadFile(s.config.seedFile)
		if err != nil {
			return fmt.Errorf("read seed %s: %w", s.config.seedFile, err)
		}
		var snapshot InventorySnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return fmt.Errorf("parse seed %s: %w", s.config.seedFile, err)
		}
		if _, err := s.fabric.Reconcile(snapshot); err != nil {
			return err
		}
		ports, _, _, _ := s.fabric.Stats()
		log.Printf("seeded from %s (%d ports)", s.config.seedFile, ports)
		return nil
	}

	const portsPerMachine = 2
	machineCount := s.config.ports / portsPerMachine
	if machineCount < 1 {
		machineCount = 1
	}
	// A plausible Mellanox/NVIDIA OUI base so GUIDs look like real hardware.
	const guidBase uint64 = 0xb8cef60300000000
	machines := make([]InventoryMachine, 0, machineCount)
	for index := 0; index < machineCount; index++ {
		ports := make([]InventoryPort, 0, portsPerMachine)
		for slot := 0; slot < portsPerMachine; slot++ {
			guid := guidBase + uint64(index*portsPerMachine+slot)
			ports = append(ports, InventoryPort{
				GUID:  fmt.Sprintf("0x%x", guid),
				State: "active",
			})
		}
		machines = append(machines, InventoryMachine{
			MatID:           "mat-sim-a",
			MachineID:       fmt.Sprintf("node-%03d", index),
			InfinibandPorts: ports,
		})
	}
	if _, err := s.fabric.Reconcile(InventorySnapshot{
		InventoryID: "sim-inventory",
		EpochID:     "epoch-1",
		Generation:  1,
		Machines:    machines,
	}); err != nil {
		return err
	}
	ports, _, _, _ := s.fabric.Stats()
	log.Printf("seeded synthetic fabric (%d ports across %d machines)", ports, machineCount)
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Subcommands let one image play all three roles in the EFS pipeline:
	// the UFM, the EFS plugin, and the Fluentd collector.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "efs":
			log.SetPrefix("[efs] ")
			runEFS()
			return
		case "sink":
			log.SetPrefix("[sink] ")
			address := env("SINK_LISTEN", "0.0.0.0:24225")
			if err := RunSink(address); err != nil {
				log.Fatalf("sink failed: %v", err)
			}
			return
		case "-h", "--help", "help":
			fmt.Println("usage: ufm-mock [efs|sink]")
			fmt.Println("  (no argument)  run the mock UFM")
			fmt.Println("  efs            run the EFS syslog forwarder")
			fmt.Println("  sink           run a Fluentd-compatible collector")
			return
		}
	}

	log.SetPrefix("[ufm-mock] ")
	cfg := loadConfig()
	srv := newServer(cfg)
	if err := srv.seed(); err != nil {
		log.Fatalf("seed failed: %v", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("UFM %s listening on %s", cfg.version, cfg.listen)
	log.Printf("auth: 'Authorization: Basic %s' or basic auth %s/%s",
		cfg.token, cfg.user, cfg.password)
	log.Printf("dashboard: http://%s/ui", cfg.listen)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
	srv.syslog.Close()
	log.Printf("stopped")
}

// runEFS starts the EFS forwarder plus its configuration API.
func runEFS() {
	efs := NewEFS()

	// Seed from the environment so a pod can start already streaming.
	config := efs.Config()
	if host := os.Getenv("EFS_SYSLOG_HOST"); host != "" {
		config.UFMSyslogEndpoint.Host = host
	}
	if port := envInt("EFS_SYSLOG_PORT", 0); port != 0 {
		config.UFMSyslogEndpoint.Port = port
	}
	if host := os.Getenv("EFS_FLUENT_HOST"); host != "" {
		config.FluentBitEndpoint.DestinationHost = host
		config.FluentBitEndpoint.Enabled = true
	}
	if port := envInt("EFS_FLUENT_PORT", 0); port != 0 {
		config.FluentBitEndpoint.DestinationPort = port
	}
	if os.Getenv("EFS_STREAMING") == "true" {
		config.Streaming.Enabled = true
	}
	seed, _ := json.Marshal(config)
	if _, err := efs.SetConfig(seed); err != nil {
		log.Fatalf("seed config: %v", err)
	}
	// Seeding queued a restart before the forwarder existed; drop it so the
	// first listener is not immediately torn down and rebuilt.
	select {
	case <-efs.restart:
	default:
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go efs.Run(ctx.Done())

	listen := env("EFS_LISTEN", "0.0.0.0:8989")
	httpServer := &http.Server{
		Addr:              listen,
		Handler:           efs.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("EFS config API on http://%s/plugin/efs/conf", listen)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("EFS API failed: %v", err)
	}
	log.Printf("stopped")
}
